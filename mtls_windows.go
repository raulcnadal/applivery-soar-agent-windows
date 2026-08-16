//go:build windows
// +build windows

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// mTLS agent authentication — Windows Agent client (Phase B). See
// backend/docs/mtls-agent-auth-roadmap.md §6 for the full design this file
// implements: local keypair generation (private key never leaves the
// device), CSR-based one-time registration via a Managed-Configuration-
// delivered bootstrap token, and a self-sustaining renewal loop that never
// needs a bootstrap token again after first enrollment.
//
// v1 keystore is deliberately file-based, not the real Windows Certificate
// Store/CNG — a disclosed simplification (roadmap §6), not a silent one.
// Files live under %ProgramData%\Applivery\SOAR\mtls\, ACL-locked to
// SYSTEM + local Administrators via icacls (restrictKeystoreACL below) —
// this agent already runs as LocalSystem for every other privileged
// operation it performs (registry policy reads, BitLocker/Firewall status,
// AppX enumeration), so that's the natural trust boundary here too.
//
// Every outbound call to the SOAR backend goes through mtlsHTTPClient/
// applyLegacyAuthIfNeeded (bottom of this file) rather than each call site
// deciding for itself — see their doc comments for why.

const mtlsDirRelative = `Applivery\SOAR\mtls`

func mtlsDir() string {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, mtlsDirRelative)
}

func mtlsCertPath() string { return filepath.Join(mtlsDir(), "device-cert.pem") }
func mtlsKeyPath() string  { return filepath.Join(mtlsDir(), "device-key.pem") }
func mtlsCaPath() string   { return filepath.Join(mtlsDir(), "ca-cert.pem") }

// ---- in-memory identity state ----

type mtlsIdentity struct {
	cert      tls.Certificate
	notBefore time.Time
	notAfter  time.Time
}

var (
	activeIdentityMu sync.RWMutex
	activeIdentity   *mtlsIdentity
)

func setActiveIdentity(id mtlsIdentity) {
	activeIdentityMu.Lock()
	defer activeIdentityMu.Unlock()
	activeIdentity = &id
}

func getActiveIdentity() *mtlsIdentity {
	activeIdentityMu.RLock()
	defer activeIdentityMu.RUnlock()
	return activeIdentity
}

// mtlsHTTPClient returns an http.Client — mTLS-configured (presents this
// device's client certificate) if a valid identity is currently loaded, a
// plain client otherwise. EVERY outbound call this agent makes to the SOAR
// backend (sendWebhook, fetchAgentStatus, forceEvaluateCompliance,
// fetchEventWatches, fetchCustomChecks) goes through this single function,
// so there is exactly one place that decides which auth mode is active —
// no call site can forget to check.
func mtlsHTTPClient(timeout time.Duration) *http.Client {
	id := getActiveIdentity()
	if id == nil {
		return &http.Client{Timeout: timeout}
	}
	// Proxy explicitly set to ProxyFromEnvironment (not left as the zero
	// value) — a bare &http.Transport{} defaults Proxy to nil/no-proxy,
	// silently dropping the HTTP_PROXY/HTTPS_PROXY support every OTHER
	// client in this agent gets for free via http.DefaultTransport. Without
	// this, a fleet behind a corporate proxy would lose connectivity the
	// moment a device completed mTLS registration — exactly the kind of
	// regression that would only show up after rollout, not in testing.
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{id.cert}},
		},
	}
}

// applyLegacyAuthIfNeeded sets the legacy X-Device-Report-Secret header —
// but ONLY when this device has no active mTLS identity loaded. Once
// registered, every request instead authenticates via the client
// certificate mtlsHTTPClient presents during the TLS handshake, and this
// header is simply omitted. Matches the roadmap's "no dual-auth code path
// to maintain on the agent side either" design (§6) — a device is either
// on legacy shared-secret auth or on mTLS, never sending both at once.
func applyLegacyAuthIfNeeded(req *http.Request, config Config) {
	if getActiveIdentity() != nil {
		return
	}
	req.Header.Set("X-Device-Report-Secret", config.ReportSecret)
}

// ---- lifecycle: load / register / renew ----

// ensureMtlsIdentity is called once at the top of every gatherAndReport
// cycle (telemetry_windows.go) — no separate ticker/goroutine needed, since
// the existing report cadence (default hourly, admin-configurable) is
// already frequent enough for a renewal window measured in days. Every step
// here is best-effort and silently falls back to whatever auth this device
// already has (legacy secret, or its current not-yet-expired certificate)
// on any failure — same tolerance as every other background operation in
// this agent (custom checks, event watches, compliance status fetch).
func ensureMtlsIdentity() {
	config := LoadConfig()
	if !config.IsConfigured() {
		return
	}
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil {
		return
	}

	if getActiveIdentity() == nil {
		if id, err := loadKeystoreIdentity(); err == nil {
			setActiveIdentity(id)
			log.Printf("mTLS: loaded existing client certificate from local keystore, valid until %s.", id.notAfter.Format(time.RFC3339))
		}
	}

	id := getActiveIdentity()
	if id == nil {
		if config.BootstrapToken != "" {
			// RegisterURL, when set, decouples first-time enrollment from the
			// mTLS vhost's health — /register never presents a client cert
			// (see Config.RegisterURL's doc comment), so it doesn't need
			// BaseURL's dedicated mTLS-configured domain at all. Falls back
			// to baseURL when RegisterURL is empty (single-URL deployments).
			registerBaseURL := baseURL
			if config.RegisterURL != "" {
				if parsed, err := url.Parse(config.RegisterURL); err == nil {
					registerBaseURL = parsed
				}
			}
			registerMtlsIdentity(registerBaseURL, config)
			return
		}
		return // no certificate yet and no bootstrap token configured — stay on legacy auth this cycle
	}

	if withinRenewalWindow(id.notBefore, id.notAfter) {
		renewMtlsIdentity(baseURL, config)
	}
}

// withinRenewalWindow triggers a renewal once less than a third of the
// certificate's own total validity period remains — scales automatically
// with whatever leafValidityDays the backend's CA is configured with
// (roadmap §3/§8: default 90 days -> renew at 30 days remaining; the
// configurable 47-day floor -> renew at ~16 days remaining), rather than a
// fixed day count that would either be too aggressive for a short-lived cert
// or dangerously late for one just above the floor.
func withinRenewalWindow(notBefore, notAfter time.Time) bool {
	validity := notAfter.Sub(notBefore)
	if validity <= 0 {
		return true
	}
	threshold := validity / 3
	return time.Until(notAfter) < threshold
}

type mtlsIssueResponse struct {
	CertPem   string `json:"certPem"`
	CaCertPem string `json:"caCertPem"`
	NotAfter  string `json:"notAfter"`
}

// registerMtlsIdentity — POST /api/device-mtls/register (roadmap §4.1).
// Deliberately uses a PLAIN http.Client (no client cert — this device
// doesn't have one yet) and authenticates via the one-time
// X-Bootstrap-Token header instead. The backend forces the issued
// certificate's CN to match the token's own bound serial number regardless
// of what CN this CSR claims (mtlsPki.go's signDeviceCsr on the backend) —
// so there's no security reliance here on this agent asking for the
// "right" identity, only on it asking with the right token.
func registerMtlsIdentity(baseURL *url.URL, config Config) {
	serialNumber := GetSerialNumber()
	if !isUsableSerial(serialNumber) {
		log.Println("mTLS: cannot register without a usable device serial number — will retry next cycle.")
		return
	}

	privKey, csrPem, err := generateMtlsKeyAndCsr(serialNumber)
	if err != nil {
		log.Printf("mTLS: failed to generate key/CSR for registration: %v", err)
		return
	}

	registerURL := baseURL.ResolveReference(&url.URL{Path: "/api/device-mtls/register"}).String()
	body, err := json.Marshal(map[string]string{"csrPem": csrPem, "serialNumber": serialNumber})
	if err != nil {
		log.Printf("mTLS: failed to marshal registration payload: %v", err)
		return
	}

	req, err := http.NewRequest("POST", registerURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("mTLS: could not build registration request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Workspace-Slug", config.WorkspaceSlug)
	req.Header.Set("X-Bootstrap-Token", config.BootstrapToken)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("mTLS: registration request failed: %v — will retry next cycle.", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("mTLS: registration rejected by backend (HTTP %d) — will retry next cycle.", resp.StatusCode)
		return
	}

	var result mtlsIssueResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("mTLS: could not parse registration response: %v", err)
		return
	}

	if err := finalizeIssuedIdentity(privKey, result); err != nil {
		log.Printf("mTLS: failed to store issued certificate: %v", err)
		return
	}
	log.Println("mTLS: registration succeeded — this device now authenticates to SOAR via client certificate.")
}

// renewMtlsIdentity — POST /api/device-mtls/renew. This is the
// self-sustaining loop from the roadmap's original spec: the request
// authenticates via the device's CURRENT, still-valid certificate
// (mtlsHTTPClient below picks it up automatically), never a bootstrap
// token, and a fresh keypair+CSR is generated for the replacement — the
// private key is never reused across renewals either.
func renewMtlsIdentity(baseURL *url.URL, config Config) {
	serialNumber := GetSerialNumber()
	if !isUsableSerial(serialNumber) {
		return
	}

	privKey, csrPem, err := generateMtlsKeyAndCsr(serialNumber)
	if err != nil {
		log.Printf("mTLS: failed to generate key/CSR for renewal: %v", err)
		return
	}

	renewURL := baseURL.ResolveReference(&url.URL{Path: "/api/device-mtls/renew"}).String()
	body, err := json.Marshal(map[string]string{"csrPem": csrPem, "serialNumber": serialNumber})
	if err != nil {
		log.Printf("mTLS: failed to marshal renewal payload: %v", err)
		return
	}

	req, err := http.NewRequest("POST", renewURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("mTLS: could not build renewal request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Workspace-Slug", config.WorkspaceSlug)

	client := mtlsHTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("mTLS: renewal request failed: %v — will retry next cycle using the still-valid current certificate.", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("mTLS: renewal rejected by backend (HTTP %d) — will retry next cycle.", resp.StatusCode)
		return
	}

	var result mtlsIssueResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("mTLS: could not parse renewal response: %v", err)
		return
	}

	if err := finalizeIssuedIdentity(privKey, result); err != nil {
		log.Printf("mTLS: failed to store renewed certificate: %v", err)
		return
	}
	log.Println("mTLS: renewal succeeded.")
}

func generateMtlsKeyAndCsr(commonName string) (*ecdsa.PrivateKey, string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate key: %w", err)
	}
	template := x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: commonName},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &template, priv)
	if err != nil {
		return nil, "", fmt.Errorf("create CSR: %w", err)
	}
	csrPem := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	return priv, csrPem, nil
}

// finalizeIssuedIdentity turns a backend register/renew response into the
// in-memory identity used by mtlsHTTPClient AND the on-disk keystore used
// to survive a restart — both updated together so the two never disagree
// about which certificate is current.
func finalizeIssuedIdentity(privKey *ecdsa.PrivateKey, result mtlsIssueResponse) error {
	keyDER, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	block, _ := pem.Decode([]byte(result.CertPem))
	if block == nil {
		return fmt.Errorf("issued certPem did not decode as PEM")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse issued certificate: %w", err)
	}

	tlsCert, err := tls.X509KeyPair([]byte(result.CertPem), keyPem)
	if err != nil {
		return fmt.Errorf("build TLS certificate pair: %w", err)
	}

	if err := writeKeystoreFileAtomic(mtlsCertPath(), []byte(result.CertPem)); err != nil {
		return err
	}
	if err := writeKeystoreFileAtomic(mtlsKeyPath(), keyPem); err != nil {
		return err
	}
	if result.CaCertPem != "" {
		if err := writeKeystoreFileAtomic(mtlsCaPath(), []byte(result.CaCertPem)); err != nil {
			log.Printf("mTLS: could not persist CA cert to keystore (non-fatal): %v", err)
		}
	}
	restrictKeystoreACL()

	setActiveIdentity(mtlsIdentity{cert: tlsCert, notBefore: parsed.NotBefore, notAfter: parsed.NotAfter})
	return nil
}

// loadKeystoreIdentity reads back whatever this agent last persisted to
// disk — called once per process lifetime (only when no in-memory identity
// is loaded yet), so a service restart doesn't force an unnecessary
// re-registration/renewal.
func loadKeystoreIdentity() (mtlsIdentity, error) {
	certPem, err := os.ReadFile(mtlsCertPath())
	if err != nil {
		return mtlsIdentity{}, err
	}
	keyPem, err := os.ReadFile(mtlsKeyPath())
	if err != nil {
		return mtlsIdentity{}, err
	}
	tlsCert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		return mtlsIdentity{}, fmt.Errorf("stored cert/key do not form a valid pair: %w", err)
	}
	block, _ := pem.Decode(certPem)
	if block == nil {
		return mtlsIdentity{}, fmt.Errorf("stored certificate file did not decode as PEM")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return mtlsIdentity{}, fmt.Errorf("could not parse stored certificate: %w", err)
	}
	if time.Now().After(parsed.NotAfter) {
		return mtlsIdentity{}, fmt.Errorf("stored certificate already expired at %s", parsed.NotAfter.Format(time.RFC3339))
	}
	return mtlsIdentity{cert: tlsCert, notBefore: parsed.NotBefore, notAfter: parsed.NotAfter}, nil
}

func writeKeystoreFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create keystore directory %s: %w", dir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write temp keystore file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename temp keystore file into place at %s: %w", path, err)
	}
	return nil
}

// restrictKeystoreACL locks the mTLS keystore directory (and everything in
// it, including the device's private key) down to SYSTEM and local
// Administrators only. This is the actual security boundary of the v1
// file-based keystore (roadmap §6's disclosed gap versus real Windows
// Certificate Store/CNG integration) — the agent already runs as
// LocalSystem for every other privileged operation, so restricting to that
// + Administrators matches the trust level everything else in this agent
// already operates at.
//
// Uses the well-known SID for Administrators (S-1-5-32-544) rather than the
// localized group name "Administrators" — the SID form works identically
// on non-English Windows installs, where the group's display name differs.
// icacls ships on every supported Windows version, so shelling out to it
// avoids pulling in a full Win32 ACL-manipulation dependency for what's
// fundamentally a one-line administrative operation.
func restrictKeystoreACL() {
	dir := mtlsDir()
	cmd := exec.Command("icacls", dir, "/inheritance:r", "/grant:r", "SYSTEM:(OI)(CI)F", "/grant:r", "*S-1-5-32-544:(OI)(CI)F")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("mTLS: could not restrict keystore ACL on %s (non-fatal, but the private key's file permissions may be broader than intended): %v (%s)", dir, err, strings.TrimSpace(string(out)))
	}
}
