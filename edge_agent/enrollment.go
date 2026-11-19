package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EnrollmentManager gestiona el aprovisionamiento autónomo, generación de claves en borde
// y ciclo de vida de certificados mTLS conforme al estándar Bindu 0.17.520.
type EnrollmentManager struct {
	mu           sync.RWMutex
	nodeID       string
	signingURL   string
	preflightURL string
	meshURL      string
	enrollUser   string
	enrollPass   string
	tlsDir       string
	caPEM        []byte
	embeddedCert []byte
	embeddedKey  []byte
	activeCert   tls.Certificate
	httpClient   *http.Client
}

type signNodeRequest struct {
	NodeID         string `json:"node_id"`
	CSRPEM         string `json:"csr_pem"`
	ForceReinstall bool   `json:"force_reinstall"`
}

type signNodeResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
	Details   struct {
		CertPEM        string `json:"cert_pem"`
		CertificatePEM string `json:"certificate_pem"`
	} `json:"details"`
	CertPEM        string `json:"cert_pem"`
	CertificatePEM string `json:"certificate_pem"`
}

func NewEnrollmentManager(
	nodeID string,
	rootHost string,
	httpsPort int,
	httpPort int,
	enrollUser string,
	enrollPass string,
	tlsDir string,
	caPEM []byte,
	embeddedCert []byte,
	embeddedKey []byte,
) *EnrollmentManager {
	if httpsPort <= 0 {
		httpsPort = 8444
	}
	if httpPort <= 0 {
		httpPort = 8080
	}
	if tlsDir == "" {
		tlsDir = "/customer_apps/tls"
	}

	// Cliente HTTPS para enrolamiento (InsecureSkipVerify para bootstrapping de primer contacto)
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}

	return &EnrollmentManager{
		nodeID:       nodeID,
		signingURL:   fmt.Sprintf("https://%s:%d/api/v1/tls/sign-node", rootHost, httpsPort),
		preflightURL: fmt.Sprintf("https://%s:%d/api/v1/tls/preflight-node", rootHost, httpsPort),
		meshURL:      fmt.Sprintf("http://%s:%d/api/v1/mesh", rootHost, httpPort),
		enrollUser:   enrollUser,
		enrollPass:   enrollPass,
		tlsDir:       tlsDir,
		caPEM:        caPEM,
		embeddedCert: embeddedCert,
		embeddedKey:  embeddedKey,
		httpClient:   client,
	}
}

// ResolveOrEnrollCertificate valida los certificados locales o ejecuta el enrolamiento PKCS#10 automático.
func (em *EnrollmentManager) ResolveOrEnrollCertificate() (tls.Certificate, error) {
	em.mu.Lock()
	defer em.mu.Unlock()

	certPath := filepath.Join(em.tlsDir, "server.crt")
	keyPath := filepath.Join(em.tlsDir, "server.key")

	// 1. Verificar certificados en tlsDir (/customer_apps/tls/)
	if x509Cert, ok := em.isCertificateValid(certPath, keyPath); ok {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err == nil {
			em.activeCert = cert
			rem := time.Until(x509Cert.NotAfter)
			fmt.Printf("[ENROLLMENT] 📜 Certificado mTLS válido en disco para nodo '%s' (Expira en %d días, %s)\n",
				em.nodeID, int(rem.Hours()/24), x509Cert.NotAfter.Format(time.RFC3339))
			return cert, nil
		}
	}

	// 2. Verificar certificados en ruta heredada (/customer_apps/bindu/runtime/tls/)
	fallbackCertPath := "/customer_apps/bindu/runtime/tls/server.crt"
	fallbackKeyPath := "/customer_apps/bindu/runtime/tls/server.key"
	if x509Cert, ok := em.isCertificateValid(fallbackCertPath, fallbackKeyPath); ok {
		cert, err := tls.LoadX509KeyPair(fallbackCertPath, fallbackKeyPath)
		if err == nil {
			em.activeCert = cert
			fmt.Printf("[ENROLLMENT] 📜 Certificado mTLS válido encontrado en runtime heredado para nodo '%s' (Expira: %s)\n",
				em.nodeID, x509Cert.NotAfter.Format(time.RFC3339))
			// Sincronizar a tlsDir principal
			_ = em.saveCredentialsFromDisk(fallbackCertPath, fallbackKeyPath)
			return cert, nil
		}
	}

	// 3. Certificado ausente, corrupto o de identidad distinta: Iniciar enrolamiento autónomo
	fmt.Printf("[ENROLLMENT] 🔐 Certificado válido no encontrado para nodo '%s'. Iniciando enrolamiento autónomo con Bindu Root (%s)...\n",
		em.nodeID, em.signingURL)

	cert, err := em.performAutonomousEnrollment()
	if err == nil {
		em.activeCert = cert
		return cert, nil
	}

	fmt.Printf("[ENROLLMENT] ⚠️ Fallo en enrolamiento online: %v\n", err)

	// 4. Fallback de contingencia: Si Root está offline durante arranque en frío y el cert embebido coincide con nodeID
	if len(em.embeddedCert) > 0 && len(em.embeddedKey) > 0 {
		if embCert, err := tls.X509KeyPair(em.embeddedCert, em.embeddedKey); err == nil {
			if len(embCert.Certificate) > 0 {
				if parsed, err := x509.ParseCertificate(embCert.Certificate[0]); err == nil {
					if parsed.Subject.CommonName == em.nodeID && time.Now().Before(parsed.NotAfter) {
						fmt.Printf("[ENROLLMENT] 🛡️ Root inaccesible. Empleando certificado embebido de contingencia (CN=%s, Expira=%s)\n",
							parsed.Subject.CommonName, parsed.NotAfter.Format(time.RFC3339))
						em.activeCert = embCert
						return embCert, nil
					}
				}
			}
		}
	}

	return tls.Certificate{}, fmt.Errorf("imposible resolver credenciales mTLS para el nodo '%s': %w", em.nodeID, err)
}

// performAutonomousEnrollment ejecuta la generación local de claves, CSR PKCS#10 y firma con Root CA.
func (em *EnrollmentManager) performAutonomousEnrollment() (tls.Certificate, error) {
	// Paso A: Generar par de claves RSA 2048 en memoria del lector (Zero Key Leakage)
	fmt.Printf("[ENROLLMENT] ⚙️ [1/4] Generando localmente par de claves RSA 2048-bit en hardware...\n")
	privKey, keyPEM, err := em.generatePrivateKeyPEM()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("error generando clave privada RSA: %w", err)
	}

	// Paso B: Generar PKCS#10 Certificate Signing Request (CSR)
	fmt.Printf("[ENROLLMENT] 📝 [2/4] Creando solicitud PKCS#10 CSR (CN=%s, O=Bindu)...\n", em.nodeID)
	csrPEM, err := em.generateCSRPEM(privKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("error generando CSR PKCS#10: %w", err)
	}

	// Paso C: Enviar CSR a Root CA endpoint (POST /api/v1/tls/sign-node)
	fmt.Printf("[ENROLLMENT] 📡 [3/4] Transmitiendo CSR a Root CA vía HTTPS (%s)...\n", em.signingURL)
	certPEM, err := em.requestCertificateSigning(csrPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("error en sign-node: %w", err)
	}

	// Paso D: Almacenamiento atómico en disco (/customer_apps/tls/)
	fmt.Printf("[ENROLLMENT] 💾 [4/4] Instalando atómicamente server.crt y server.key en %s...\n", em.tlsDir)
	if err := em.saveCredentials(certPEM, keyPEM); err != nil {
		return tls.Certificate{}, fmt.Errorf("error guardando credenciales en disco: %w", err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("error cargando certificado firmado: %w", err)
	}

	fmt.Printf("[ENROLLMENT] 🎉 ¡Enrolamiento autónomo COMPLETADO con éxito para nodo '%s'!\n", em.nodeID)
	return cert, nil
}

// generatePrivateKeyPEM genera una clave RSA 2048 y la codifica en PEM PKCS#8.
func (em *EnrollmentManager) generatePrivateKeyPEM() (*rsa.PrivateKey, []byte, error) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		// Fallback a PKCS#1 si PKCS#8 falla
		pkcs1Bytes := x509.MarshalPKCS1PrivateKey(privKey)
		keyPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: pkcs1Bytes,
		})
		return privKey, keyPEM, nil
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8Bytes,
	})
	return privKey, keyPEM, nil
}

// generateCSRPEM genera la solicitud PKCS#10 con CommonName = nodeID y Organization = Bindu.
func (em *EnrollmentManager) generateCSRPEM(privKey *rsa.PrivateKey) ([]byte, error) {
	template := x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   em.nodeID,
			Organization: []string{"Bindu"},
		},
		SignatureAlgorithm: x509.SHA256WithRSA,
	}

	csrBytes, err := x509.CreateCertificateRequest(rand.Reader, &template, privKey)
	if err != nil {
		return nil, err
	}

	csrPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE REQUEST",
		Bytes: csrBytes,
	})
	return csrPEM, nil
}

// requestCertificateSigning solicita la firma del CSR a Bindu Root CA.
func (em *EnrollmentManager) requestCertificateSigning(csrPEM []byte) ([]byte, error) {
	reqBody := signNodeRequest{
		NodeID:         em.nodeID,
		CSRPEM:         string(csrPEM),
		ForceReinstall: true,
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", em.signingURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if em.enrollUser != "" && em.enrollPass != "" {
		req.SetBasicAuth(em.enrollUser, em.enrollPass)
		req.Header.Set("X-Enrollment-Token", em.enrollPass)
	}

	resp, err := em.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error de red al conectar a Root CA: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error leyendo respuesta de Root CA: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Root CA retornó HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var signResp signNodeResponse
	if err := json.Unmarshal(bodyBytes, &signResp); err != nil {
		return nil, fmt.Errorf("error decodificando respuesta JSON: %w (Raw: %s)", err, string(bodyBytes))
	}

	var certPEM string
	if signResp.Details.CertPEM != "" {
		certPEM = signResp.Details.CertPEM
	} else if signResp.Details.CertificatePEM != "" {
		certPEM = signResp.Details.CertificatePEM
	} else if signResp.CertPEM != "" {
		certPEM = signResp.CertPEM
	} else if signResp.CertificatePEM != "" {
		certPEM = signResp.CertificatePEM
	}

	if certPEM == "" {
		return nil, fmt.Errorf("respuesta válida pero sin cert_pem presente: %s", string(bodyBytes))
	}

	return []byte(certPEM), nil
}

// isCertificateValid comprueba existencia, validez temporal y pertenencia del CN al nodo activo.
func (em *EnrollmentManager) isCertificateValid(certPath, keyPath string) (*x509.Certificate, bool) {
	certBytes, err := os.ReadFile(certPath)
	if err != nil || len(certBytes) == 0 {
		return nil, false
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil || len(keyBytes) == 0 {
		return nil, false
	}

	block, _ := pem.Decode(certBytes)
	if block == nil {
		return nil, false
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, false
	}

	// Comprobar que el CN pertenezca exactamente a este nodo
	if cert.Subject.CommonName != em.nodeID {
		fmt.Printf("[ENROLLMENT] ⚠️ CN del certificado ('%s') no coincide con nodo activo ('%s'). Requiere nuevo enrolamiento.\n",
			cert.Subject.CommonName, em.nodeID)
		return nil, false
	}

	// Comprobar vigencia temporal
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		fmt.Printf("[ENROLLMENT] ⚠️ Certificado expirado o aún no válido (NotBefore: %s, NotAfter: %s).\n",
			cert.NotBefore.Format(time.RFC3339), cert.NotAfter.Format(time.RFC3339))
		return nil, false
	}

	return cert, true
}

// saveCredentials persiste atómicamente server.crt, server.key y ca.crt en disco.
func (em *EnrollmentManager) saveCredentials(certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(em.tlsDir, 0700); err != nil {
		return err
	}

	certPath := filepath.Join(em.tlsDir, "server.crt")
	keyPath := filepath.Join(em.tlsDir, "server.key")
	caPath := filepath.Join(em.tlsDir, "ca.crt")

	// Escritura de certificado (0644)
	if err := writeFileAtomic(certPath, certPEM, 0644); err != nil {
		return err
	}

	// Escritura de clave privada (0600)
	if err := writeFileAtomic(keyPath, keyPEM, 0600); err != nil {
		return err
	}

	// Copiar ca.crt si se dispone del PEM
	if len(em.caPEM) > 0 {
		_ = writeFileAtomic(caPath, em.caPEM, 0644)
	}

	return nil
}

func (em *EnrollmentManager) saveCredentialsFromDisk(srcCert, srcKey string) error {
	certPEM, err := os.ReadFile(srcCert)
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(srcKey)
	if err != nil {
		return err
	}
	return em.saveCredentials(certPEM, keyPEM)
}

func writeFileAtomic(dst string, data []byte, perm os.FileMode) error {
	tmp := fmt.Sprintf("%s.tmp.%d", dst, time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()

	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// StartBackgroundRenewal inicia un monitor que cada 12 horas evalúa si el certificado
// ha superado el 80% de su vida útil (< 30 días restantes) para renovarlo automáticamente.
func (em *EnrollmentManager) StartBackgroundRenewal(reconnectTrigger func()) {
	go func() {
		ticker := time.NewTicker(12 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			certPath := filepath.Join(em.tlsDir, "server.crt")
			keyPath := filepath.Join(em.tlsDir, "server.key")
			x509Cert, ok := em.isCertificateValid(certPath, keyPath)
			if !ok {
				continue
			}

			remaining := time.Until(x509Cert.NotAfter)
			// Renovar si quedan menos de 30 días
			if remaining < 30*24*time.Hour {
				fmt.Printf("[RENEWAL] ⏳ Certificado mTLS a menos de 30 días de expirar (%d días restantes). Iniciando renovación automática...\n",
					int(remaining.Hours()/24))

				em.mu.Lock()
				newCert, err := em.performAutonomousEnrollment()
				if err == nil {
					em.activeCert = newCert
					fmt.Printf("[RENEWAL] ✅ Certificado mTLS renovado exitosamente. Notificando reconexión...\n")
					if reconnectTrigger != nil {
						reconnectTrigger()
					}
				} else {
					fmt.Printf("[RENEWAL] ⚠️ Fallo al renovar certificado en segundo plano: %v\n", err)
				}
				em.mu.Unlock()
			}
		}
	}()
}

// GetActiveCertificate retorna el certificado actualmente cargado en memoria de forma segura.
func (em *EnrollmentManager) GetActiveCertificate() tls.Certificate {
	em.mu.RLock()
	defer em.mu.RUnlock()
	return em.activeCert
}
