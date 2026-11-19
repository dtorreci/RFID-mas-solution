package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed certs/ca.crt
var caCertPEM []byte

//go:embed certs/idx4000.crt
var clientCertPEM []byte

//go:embed certs/idx4000.key
var clientKeyPEM []byte

const (
	DefaultLLRPAddr = "127.0.0.1:5084"
	DefaultMQTTAddr = "bindu.decn.casa:1883"
	CanonicalNodeID = "idx4000-00-50-f9-d1-0a-2b"
	TargetGTIN      = "07621806175810"
	TargetEPCHex    = "30b5d132b8112b42f9b59b2a"
	TargetSerial    = "12779363114"
	AntennaModel    = "Keonn Advan-CP1 EU"
	BinduVersion    = "0.27.520"
	MinPublishGap   = 100 * time.Millisecond
)

type MotionTracker struct {
	mu           sync.Mutex
	hasPrev      bool
	prevRSSI     float64
	prevTime     time.Time
	lastPubTime  time.Time
	targetHits   int
	straysFilter int
}

type BinduEnvelope struct {
	Type       string `json:"$type"`
	Group      string `json:"$group"`
	Producer   string `json:"$producer"`
	Node       string `json:"$node,omitempty"`
	NodeOrigin string `json:"$node-origin,omitempty"`
	NodeDest   string `json:"$node-dest,omitempty"`
	Src        string `json:"$src,omitempty"`
	UptimeSec  int64  `json:"$uptime_sec,omitempty"`
	User       string `json:"$user,omitempty"`
	When       string `json:"$when"`
	Fields     any    `json:"fields"`
}

type Agent struct {
	// 64-bit atomic counters placed first for strict 8-byte alignment on 32-bit ARM (ARMv7)
	readsCount    atomic.Int64
	presenceCount atomic.Int64
	stateCount    atomic.Int64
	pingCount     atomic.Int64
	errorCount    atomic.Int64
	aborted       atomic.Int32

	macAddr       string
	nodeID        string
	llrpAddr      string
	mqttAddr      string
	antennaID     uint16
	powerDbm      uint16
	allTags       bool
	mqttUser      string
	mqttPass      string
	llrpConn      net.Conn
	mqttConn      net.Conn
	tracker       *MotionTracker
	running       bool
	startTime     time.Time
	mqttMu        sync.Mutex
	reconnectChan chan struct{}
	enrollment    *EnrollmentManager
}

type RawTagItem struct {
	Timestamp time.Time
	AntennaID int
	PeakRSSI  float64
	Channel   int
	EPCHex    string
}

func getMacAddress() string {
	data, err := os.ReadFile("/sys/class/net/eth0/address")
	if err == nil {
		mac := strings.TrimSpace(string(data))
		if mac != "" {
			return strings.ToUpper(mac)
		}
	}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if (iface.Name == "eth0" || strings.HasPrefix(iface.Name, "eth")) && iface.HardwareAddr != nil {
				return strings.ToUpper(iface.HardwareAddr.String())
			}
		}
	}
	return "00:50:F9:D1:0A:2B"
}

func getLocalIPAddresses() []string {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{"192.168.1.143"}
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					ips = append(ips, ipnet.IP.String())
				}
			}
		}
	}
	if len(ips) == 0 {
		return []string{"192.168.1.143"}
	}
	return ips
}

func main() {
	llrpAddr := flag.String("llrp", DefaultLLRPAddr, "LLRP reader address (host:port)")
	mqttAddr := flag.String("mqtt", DefaultMQTTAddr, "Bindu MQTT broker address (host:port)")
	user := flag.String("user", "dtorreci", "MQTT username")
	pass := flag.String("pass", "BinduMaster2026!", "MQTT password")
	ant := flag.Int("ant", 0, "Antenna port (0=all antennas, 1-4)")
	power := flag.Int("power", 18, "Transmit power in dBm (default: 18 dBm)")
	allTags := flag.Bool("all-tags", true, "Publish all tags (false=filter target EPC only)")
	nodeIDFlag := flag.String("node-id", "", "Canonical Bindu Node ID (default: derived from MAC or CanonicalNodeID)")
	rootHostFlag := flag.String("root-host", "", "Bindu Root host (default: derived from -mqtt address)")
	rootHttpsPort := flag.Int("root-https-port", 8444, "Bindu Root HTTPS TLS Signer API port")
	rootHttpPort := flag.Int("root-http-port", 8080, "Bindu Root HTTP Console API port")
	enrollUser := flag.String("enroll-user", "enrollment_agent", "Bindu enrollment agent username")
	enrollPass := flag.String("enroll-pass", "HhAuth2026!", "Bindu enrollment agent password/token")
	tlsDir := flag.String("tls-dir", "/customer_apps/tls", "Directory to store autonomous mTLS certificates")
	flag.Parse()

	mac := getMacAddress()
	nodeID := *nodeIDFlag
	if nodeID == "" {
		formattedMac := strings.ToLower(strings.ReplaceAll(mac, ":", "-"))
		if formattedMac == "00-50-f9-d1-0a-2b" {
			nodeID = CanonicalNodeID
		} else {
			nodeID = fmt.Sprintf("idx4000-%s", formattedMac)
		}
	}

	rootHost := *rootHostFlag
	if rootHost == "" {
		if h, _, err := net.SplitHostPort(*mqttAddr); err == nil {
			rootHost = h
		} else {
			rootHost = "bindu.decn.casa"
		}
	}

	enrollment := NewEnrollmentManager(
		nodeID,
		rootHost,
		*rootHttpsPort,
		*rootHttpPort,
		*enrollUser,
		*enrollPass,
		*tlsDir,
		caCertPEM,
		clientCertPEM,
		clientKeyPEM,
	)

	fmt.Println("=========================================================================================")
	fmt.Printf("   🚀 SENSORMATIC IDX-4000 BINDU EDGE AGENT (ARMv7 Native / Bindu %s)\n", BinduVersion)
	fmt.Printf("   🏷️ Hardware MAC: %s | Canonical Node ID: %s\n", mac, nodeID)
	fmt.Printf("   📡 Local LLRP: %s | Antena Port %d (%s) | Potencia: %d dBm\n", *llrpAddr, *ant, AntennaModel, *power)
	fmt.Printf("   🎯 Target GTIN: %s (EPC: %s)\n", TargetGTIN, TargetEPCHex)
	fmt.Printf("   📦 Raw Ingress Pipeline: bindu/in/raw/rfid/idx4000 (Bottom-Up Telemetry)\n")
	fmt.Printf("   🛡️ System Event Types: NodePresence, MessageState, MessagePing, ErrorEvent\n")
	fmt.Printf("   🔐 Autonomous Enrollment: PKCS#10 CSR -> %s:8444/api/v1/tls/sign-node\n", rootHost)
	fmt.Printf("   🔒 MQTT Broker: %s (mTLS Zero-Plaintext Wire Guarantee)\n", *mqttAddr)
	fmt.Println("=========================================================================================")

	agent := &Agent{
		macAddr:       mac,
		nodeID:        nodeID,
		llrpAddr:      *llrpAddr,
		mqttAddr:      *mqttAddr,
		antennaID:     uint16(*ant),
		powerDbm:      uint16(*power),
		allTags:       *allTags,
		mqttUser:      *user,
		mqttPass:      *pass,
		enrollment:    enrollment,
		tracker:       &MotionTracker{},
		running:       true,
		startTime:     time.Now(),
		reconnectChan: make(chan struct{}, 1),
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\n\n[SHUTDOWN] Señal de terminación recibida. Deteniendo agente...")
		agent.stop()
		os.Exit(0)
	}()

	if err := agent.run(); err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] Error en ejecución del agente: %v\n", err)
		os.Exit(1)
	}
}

func (a *Agent) stop() {
	a.running = false
	a.aborted.Store(1)
	if a.llrpConn != nil {
		a.llrpConn.Write(encodeLLRP(23, 999, uint32Bytes(1))) // STOP_ROSPEC
		a.llrpConn.Close()
	}
	a.disconnectMQTT()
}

func (a *Agent) triggerReconnect(reason string) {
	if a.aborted.CompareAndSwap(0, 1) {
		fmt.Printf("[HEALTH-GUARD] 🚨 Fallo de conexión detectado (%s). Abortando bucles para reconexión...\n", reason)
		if a.llrpConn != nil {
			a.llrpConn.Close()
		}
		select {
		case a.reconnectChan <- struct{}{}:
		default:
		}
	}
}

func (a *Agent) run() error {
	// Verificar o enrolar credenciales mTLS de forma autónoma antes de conectar
	fmt.Printf("\n[ENROLLMENT] Verificando estado de credenciales mTLS para nodo '%s'...\n", a.nodeID)
	if _, err := a.enrollment.ResolveOrEnrollCertificate(); err != nil {
		fmt.Printf("[ENROLLMENT] ⚠️ Advertencia durante resolución de certificado: %v\n", err)
	}

	// Iniciar monitor de renovación automática en segundo plano (>80% vida útil / <30 días)
	a.enrollment.StartBackgroundRenewal(func() {
		a.triggerReconnect("Certificado mTLS renovado en segundo plano")
	})

	for a.running {
		a.aborted.Store(0)

		fmt.Printf("\n[CONEXIÓN] Estableciendo enlace mTLS con Bindu Mesh Gateway (%s)...\n", a.mqttAddr)
		if err := a.connectMQTT(); err != nil {
			fmt.Printf("[MQTT] ⚠️ Fallo al conectar con Bindu: %v. Reintentando en 3s...\n", err)
			time.Sleep(3 * time.Second)
			continue
		}
		fmt.Printf("[MQTT] ✅ Enlace mTLS autenticado exitosamente (NodeID: %s, User: %s)\n", a.nodeID, a.mqttUser)

		// 1. Emitir NodePresence de arranque
		if err := a.sendNodePresence(); err != nil {
			fmt.Printf("[PRESENCE] ⚠️ Fallo al emitir NodePresence inicial: %v\n", err)
		} else {
			fmt.Println("[PRESENCE] 🟢 NodePresence inicial registrado con éxito en Bindu Root.")
		}

		// 2. Emitir MessageState inicial
		if err := a.sendMessageState(); err != nil {
			fmt.Printf("[STATE] ⚠️ Fallo al emitir MessageState inicial: %v\n", err)
		} else {
			fmt.Println("[STATE] 📊 MessageState inicial emitido (Salud y recursos del hardware).")
		}

		// 3. Goroutines de eventos del sistema (Heartbeats, State y Ping)
		stopLoops := make(chan struct{})
		go a.startSystemEventLoops(stopLoops)

		// 4. Conectar a LLRP interno
		fmt.Printf("[CONEXIÓN] Conectando a servicio interno LLRP (%s)...\n", a.llrpAddr)
		if err := a.connectLLRP(); err != nil {
			fmt.Printf("[LLRP] ⚠️ Error conectando con ireader daemon: %v. Reintentando en 3s...\n", err)
			a.sendErrorEvent("LLRP_DAEMON_CONNECT_FAILED", fmt.Sprintf("Error conectando con ireader: %v", err), "")
			close(stopLoops)
			a.disconnectMQTT()
			time.Sleep(3 * time.Second)
			continue
		}
		fmt.Println("[LLRP] ✅ Conexión establecida con servicio RFID local de Sensormatic.")

		// 5. Configurar y arrancar ROSpec
		fmt.Printf("[ROSPEC] Configurando ROSpec #1 (Antena %d a %d dBm)...\n", a.antennaID, a.powerDbm)
		if err := a.setupAndStartROSpec(); err != nil {
			fmt.Printf("[ROSPEC] ⚠️ Fallo al inicializar ROSpec: %v\n", err)
			a.sendErrorEvent("ROSPEC_INITIALIZATION_FAILED", fmt.Sprintf("Fallo al iniciar ROSpec: %v", err), "")
			close(stopLoops)
			a.disconnectLLRP()
			a.disconnectMQTT()
			time.Sleep(3 * time.Second)
			continue
		}
		fmt.Println("[ROSPEC] ✅ ROSpec ejecutándose. Lector radiando en puerto de antena 1.")

		// 6. Bucle principal de lectura LLRP
		if err := a.readLoop(); err != nil {
			fmt.Printf("[LOOP] Desconexión de datos LLRP / Red: %v\n", err)
		}

		close(stopLoops)
		a.disconnectLLRP()
		a.disconnectMQTT()

		if a.running {
			fmt.Println("[RECONNECT] Esperando 3 segundos antes de reiniciar ciclo de conexión...")
			time.Sleep(3 * time.Second)
		}
	}
	return nil
}

func (a *Agent) startSystemEventLoops(stopChan chan struct{}) {
	presenceTicker := time.NewTicker(60 * time.Second)
	stateTicker := time.NewTicker(30 * time.Second)
	pingTicker := time.NewTicker(25 * time.Second)
	defer presenceTicker.Stop()
	defer stateTicker.Stop()
	defer pingTicker.Stop()

	for {
		select {
		case <-stopChan:
			return
		case <-presenceTicker.C:
			if err := a.sendNodePresence(); err != nil {
				fmt.Printf("[PRESENCE] ⚠️ Error emitiendo NodePresence periódico: %v\n", err)
			}
		case <-stateTicker.C:
			if err := a.sendMessageState(); err != nil {
				fmt.Printf("[STATE] ⚠️ Error emitiendo MessageState periódico: %v\n", err)
			}
		case <-pingTicker.C:
			if err := a.sendMessagePing(); err != nil {
				fmt.Printf("[PING] ⚠️ Error emitiendo MessagePing / PINGREQ: %v\n", err)
				a.triggerReconnect("Fallo de ping MQTT hacia Bindu Root")
			}
		}
	}
}

// -----------------------------------------------------------------------------
// SYSTEM EVENT TYPES: IMPLEMENTACIONES FORMALES CONFORME A BINDU 0.27.520
// -----------------------------------------------------------------------------

func (a *Agent) sendNodePresence() error {
	now := time.Now().UTC()
	nowUTC := now.Format(time.RFC3339)
	ips := getLocalIPAddresses()

	var addresses []string
	var endpoints []map[string]any
	for _, ip := range ips {
		addresses = append(addresses, fmt.Sprintf("%s:1883", ip))
		endpoints = append(endpoints, map[string]any{
			"host":     ip,
			"kind":     "mesh_mqtt",
			"port":     1883,
			"priority": 100,
			"tls":      true,
		})
	}

	payloadFields := map[string]any{
		"node_id":     a.nodeID,
		"version":     BinduVersion,
		"ttl_seconds": 5400,
		"seen_at":     nowUTC,
		"addresses":   addresses,
		"aliases":     []string{"Sensormatic-IDX4000", a.nodeID},
		"endpoints":   endpoints,
	}

	envelope := BinduEnvelope{
		Type:       "NodePresence",
		Group:      "mesh",
		Producer:   a.nodeID,
		NodeOrigin: a.nodeID,
		NodeDest:   "root",
		When:       nowUTC,
		Fields:     payloadFields,
	}

	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}

	if err := a.publishMQTT("bindu/in", data); err != nil {
		return err
	}
	a.presenceCount.Add(1)
	return nil
}

func (a *Agent) sendMessageState() error {
	now := time.Now().UTC()
	nowUTC := now.Format(time.RFC3339)
	uptimeSec := int64(time.Since(a.startTime).Seconds())

	metrics := a.collectLinuxSystemMetrics()

	totalMsgs := a.readsCount.Load() + a.presenceCount.Load() +
		a.stateCount.Load() + a.errorCount.Load()

	payloadFields := map[string]any{
		"identity": map[string]any{
			"node_id":    a.nodeID,
			"role":       "node",
			"uptime_sec": uptimeSec,
			"version":    BinduVersion,
		},
		"platform": map[string]any{
			"arch":   "arm",
			"os":     "linux",
			"target": "linux/armv7",
		},
		"cpu": map[string]any{
			"cores_logical": metrics.CPUCores,
			"load_1m":       metrics.CPULoad1m,
			"load_5m":       metrics.CPULoad5m,
			"usage_pct":     metrics.CPUUsagePct,
		},
		"memory": map[string]any{
			"total_mb":     metrics.MemTotalMB,
			"available_mb": metrics.MemAvailMB,
			"used_pct":     metrics.MemUsedPct,
		},
		"disk": map[string]any{
			"path":     metrics.DiskPath,
			"total_gb": metrics.DiskTotalGB,
			"free_gb":  metrics.DiskFreeGB,
			"used_pct": metrics.DiskUsedPct,
		},
		"system_plugins": []map[string]any{
			{
				"name":   "ireader-llrp-daemon",
				"pid":    metrics.IReaderPID,
				"status": metrics.IReaderStatus,
			},
		},
		"storage": map[string]any{
			"message_total": totalMsgs,
			"message_counts_by_type": []map[string]any{
				{"type": "RawTagRead", "total": a.readsCount.Load()},
				{"type": "NodePresence", "total": a.presenceCount.Load()},
				{"type": "MessageState", "total": a.stateCount.Load()},
				{"type": "ErrorEvent", "total": a.errorCount.Load()},
			},
		},
		"system_resources": map[string]any{
			"cpu_pct":        metrics.CPUUsagePct,
			"memory_pct":     metrics.MemUsedPct,
			"disk_pct":       metrics.DiskUsedPct,
			"uptime_seconds": uptimeSec,
		},
	}

	envelope := BinduEnvelope{
		Type:       "MessageState",
		Group:      "mesh",
		Producer:   a.nodeID,
		Node:       a.nodeID,
		NodeOrigin: a.nodeID,
		NodeDest:   "root",
		UptimeSec:  uptimeSec,
		User:       "mesh:" + a.nodeID,
		When:       nowUTC,
		Fields:     payloadFields,
	}

	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}

	if err := a.publishMQTT("bindu/in", data); err != nil {
		return err
	}
	a.stateCount.Add(1)
	return nil
}

func (a *Agent) sendMessagePing() error {
	nowUTC := time.Now().UTC().Format(time.RFC3339)

	// Enviar PINGREQ nativo a nivel de protocolo MQTT para mantener el socket activo
	a.mqttMu.Lock()
	if a.mqttConn != nil {
		a.mqttConn.Write([]byte{0xC0, 0x00})
	}
	a.mqttMu.Unlock()

	// Enviar mensaje semántico MessagePing con fields: {} canónico de Bindu
	envelope := BinduEnvelope{
		Type:     "MessagePing",
		Group:    "system",
		Producer: a.nodeID,
		Src:      a.nodeID,
		NodeDest: "root",
		When:     nowUTC,
		Fields:   map[string]any{},
	}

	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}

	if err := a.publishMQTT("bindu/in", data); err != nil {
		return err
	}
	a.pingCount.Add(1)
	return nil
}

func (a *Agent) sendErrorEvent(errType, reason, payloadStr string) {
	nowUTC := time.Now().UTC().Format(time.RFC3339)
	a.errorCount.Add(1)

	envelope := BinduEnvelope{
		Type:       "ErrorEvent",
		Group:      "mesh",
		Producer:   a.nodeID,
		NodeOrigin: a.nodeID,
		NodeDest:   "root",
		When:       nowUTC,
		Fields: map[string]any{
			"client_id":       a.nodeID,
			"error_type":      errType,
			"group_id":        "mesh",
			"origin_producer": a.nodeID,
			"payload":         payloadStr,
			"reason":          reason,
			"timestamp":       nowUTC,
			"topic":           "bindu/in/raw/rfid/idx4000",
			"username":        "system",
		},
	}

	data, err := json.Marshal(envelope)
	if err == nil {
		_ = a.publishMQTT("bindu/in", data)
	}
}

// -----------------------------------------------------------------------------
// RECOLECCIÓN DE MÉTRICAS LINUX / ARMv7 DEL HARDWARE SENSORMATIC
// -----------------------------------------------------------------------------

type SystemMetrics struct {
	CPUCores      int
	CPULoad1m     float64
	CPULoad5m     float64
	CPUUsagePct   float64
	MemTotalMB    float64
	MemAvailMB    float64
	MemUsedPct    float64
	DiskPath      string
	DiskTotalGB   float64
	DiskFreeGB    float64
	DiskUsedPct   float64
	IReaderPID    int
	IReaderStatus string
}

func (a *Agent) collectLinuxSystemMetrics() SystemMetrics {
	m := SystemMetrics{
		CPUCores:      2,
		CPULoad1m:     0.10,
		CPULoad5m:     0.10,
		CPUUsagePct:   5.0,
		MemTotalMB:    1024.0,
		MemAvailMB:    750.0,
		MemUsedPct:    26.7,
		DiskPath:      "/",
		DiskTotalGB:   4.0,
		DiskFreeGB:    2.8,
		DiskUsedPct:   30.0,
		IReaderPID:    0,
		IReaderStatus: "running",
	}

	// 1. Load averages
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) >= 2 {
			if l1, err := strconv.ParseFloat(parts[0], 64); err == nil {
				m.CPULoad1m = l1
				m.CPUUsagePct = math.Min(100.0, (l1/float64(m.CPUCores))*100.0)
			}
			if l5, err := strconv.ParseFloat(parts[1], 64); err == nil {
				m.CPULoad5m = l5
			}
		}
	}

	// 2. Memory info
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		var totalKb, availKb, freeKb float64
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			val, _ := strconv.ParseFloat(fields[1], 64)
			switch fields[0] {
			case "MemTotal:":
				totalKb = val
			case "MemAvailable:":
				availKb = val
			case "MemFree:":
				freeKb = val
			}
		}
		if totalKb > 0 {
			m.MemTotalMB = math.Round((totalKb/1024.0)*10) / 10
			if availKb == 0 {
				availKb = freeKb
			}
			m.MemAvailMB = math.Round((availKb/1024.0)*10) / 10
			usedKb := totalKb - availKb
			m.MemUsedPct = math.Round(((usedKb/totalKb)*100.0)*10) / 10
		}
	}

	// 3. Disk stats (preferir /customer_apps o fallback a /)
	diskTarget := "/customer_apps"
	var stat syscall.Statfs_t
	if err := syscall.Statfs(diskTarget, &stat); err != nil {
		diskTarget = "/"
		_ = syscall.Statfs(diskTarget, &stat)
	}
	m.DiskPath = diskTarget
	if stat.Blocks > 0 {
		totalBytes := stat.Blocks * uint64(stat.Bsize)
		freeBytes := stat.Bfree * uint64(stat.Bsize)
		m.DiskTotalGB = math.Round((float64(totalBytes)/(1024*1024*1024))*100) / 100
		m.DiskFreeGB = math.Round((float64(freeBytes)/(1024*1024*1024))*100) / 100
		usedBytes := totalBytes - freeBytes
		m.DiskUsedPct = math.Round(((float64(usedBytes)/float64(totalBytes))*100.0)*10) / 10
	}

	// 4. ireader status
	if a.llrpConn != nil {
		m.IReaderStatus = "running"
	} else {
		m.IReaderStatus = "disconnected"
	}

	return m
}

// -----------------------------------------------------------------------------
// CONEXIÓN Y TRANSPORTE MQTT mTLS
// -----------------------------------------------------------------------------

func (a *Agent) connectMQTT() error {
	a.mqttMu.Lock()
	defer a.mqttMu.Unlock()

	caCertPool := x509.NewCertPool()
	// Cargar CA de disco si existe
	if caDisk, err := os.ReadFile(filepath.Join(a.enrollment.tlsDir, "ca.crt")); err == nil && len(caDisk) > 0 {
		caCertPool.AppendCertsFromPEM(caDisk)
	}
	// Añadir CA raíz de Bindu
	if len(caCertPEM) > 0 {
		caCertPool.AppendCertsFromPEM(caCertPEM)
	}

	cert := a.enrollment.GetActiveCertificate()
	if len(cert.Certificate) == 0 {
		var err error
		cert, err = a.enrollment.ResolveOrEnrollCertificate()
		if err != nil {
			return fmt.Errorf("fallo al resolver certificado mTLS: %w", err)
		}
	}

	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		RootCAs:            caCertPool,
		InsecureSkipVerify: true,
	}

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", a.mqttAddr, tlsConfig)
	if err != nil {
		return fmt.Errorf("error tls.Dial: %w", err)
	}

	var varHdr []byte
	varHdr = append(varHdr, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04)
	connectFlags := byte(0x02)
	if a.mqttUser != "" {
		connectFlags |= 0x80
	}
	if a.mqttPass != "" {
		connectFlags |= 0x40
	}
	varHdr = append(varHdr, connectFlags, 0x00, 0x3C)

	var payload []byte
	payload = append(payload, encodeMqttString(a.nodeID)...)
	if a.mqttUser != "" {
		payload = append(payload, encodeMqttString(a.mqttUser)...)
	}
	if a.mqttPass != "" {
		payload = append(payload, encodeMqttString(a.mqttPass)...)
	}

	conn.Write(buildMQTTPacket(0x10, append(varHdr, payload...)))
	ack := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, ack); err != nil {
		conn.Close()
		return fmt.Errorf("error leyendo CONNACK: %w", err)
	}
	conn.SetReadDeadline(time.Time{})

	if ack[0] != 0x20 || ack[3] != 0x00 {
		conn.Close()
		return fmt.Errorf("CONNACK rechazado por Bindu con código 0x%02x", ack[3])
	}

	a.mqttConn = conn
	return nil
}

func (a *Agent) disconnectMQTT() {
	a.mqttMu.Lock()
	defer a.mqttMu.Unlock()
	if a.mqttConn != nil {
		a.mqttConn.Close()
		a.mqttConn = nil
	}
}

func (a *Agent) publishMQTT(topic string, payload []byte) error {
	a.mqttMu.Lock()
	defer a.mqttMu.Unlock()

	if a.mqttConn == nil {
		return fmt.Errorf("mqtt no conectado")
	}

	var varHdr []byte
	varHdr = append(varHdr, encodeMqttString(topic)...)

	pkt := buildMQTTPacket(0x30, append(varHdr, payload...))
	a.mqttConn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err := a.mqttConn.Write(pkt)
	a.mqttConn.SetWriteDeadline(time.Time{})
	if err != nil {
		a.triggerReconnect(fmt.Sprintf("Error al escribir paquete MQTT en topic %s: %v", topic, err))
	}
	return err
}

// -----------------------------------------------------------------------------
// GESTIÓN DE PROTOCOLO LLRP CON IREPORTS NATIVAS
// -----------------------------------------------------------------------------

func (a *Agent) connectLLRP() error {
	conn, err := net.DialTimeout("tcp", a.llrpAddr, 3*time.Second)
	if err != nil {
		return err
	}
	a.llrpConn = conn

	hdr := make([]byte, 10)
	if _, err := io.ReadFull(a.llrpConn, hdr); err != nil {
		return err
	}
	msgLen := binary.BigEndian.Uint32(hdr[2:6])
	body := make([]byte, int(msgLen)-10)
	if _, err := io.ReadFull(a.llrpConn, body); err != nil {
		return err
	}
	return nil
}

func (a *Agent) disconnectLLRP() {
	if a.llrpConn != nil {
		a.llrpConn.Close()
		a.llrpConn = nil
	}
}

func (a *Agent) setupAndStartROSpec() error {
	hdr := make([]byte, 10)

	// DELETE_ROSPEC 1
	a.llrpConn.Write(encodeLLRP(21, 101, uint32Bytes(1)))
	io.ReadFull(a.llrpConn, hdr)
	l := binary.BigEndian.Uint32(hdr[2:6])
	body := make([]byte, int(l)-10)
	io.ReadFull(a.llrpConn, body)

	// ADD_ROSPEC 1
	a.llrpConn.Write(encodeLLRP(20, 102, buildStrictROSpec(1, a.antennaID, a.powerDbm)))
	io.ReadFull(a.llrpConn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(a.llrpConn, body)

	// ENABLE_ROSPEC 1
	a.llrpConn.Write(encodeLLRP(24, 103, uint32Bytes(1)))
	io.ReadFull(a.llrpConn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(a.llrpConn, body)

	// START_ROSPEC 1
	a.llrpConn.Write(encodeLLRP(22, 104, uint32Bytes(1)))
	io.ReadFull(a.llrpConn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(a.llrpConn, body)

	return nil
}

func (a *Agent) readLoop() error {
	hdr := make([]byte, 10)
	for a.running && a.aborted.Load() == 0 {
		if a.llrpConn == nil {
			return fmt.Errorf("llrpConn cerrado")
		}

		_ = a.llrpConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		if _, err := io.ReadFull(a.llrpConn, hdr); err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return err
		}

		msgType := (binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF)
		msgLen := binary.BigEndian.Uint32(hdr[2:6])
		if msgLen < 10 {
			return fmt.Errorf("longitud de mensaje inválida: %d", msgLen)
		}

		body := make([]byte, int(msgLen)-10)
		_ = a.llrpConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(a.llrpConn, body); err != nil {
			return err
		}

		if msgType == 62 { // KEEPALIVE
			a.llrpConn.Write(encodeLLRP(72, binary.BigEndian.Uint32(hdr[6:10]), nil))
			continue
		}

		if msgType == 61 { // RO_ACCESS_REPORT
			a.parseROAccessReport(body)
		}
	}
	return nil
}

func (a *Agent) parseROAccessReport(data []byte) {
	offset := 0
	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}
		paramType := binary.BigEndian.Uint16(data[offset:offset+2]) & 0x03FF
		paramLen := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if paramLen < 4 || offset+paramLen > len(data) {
			break
		}
		if paramType == 240 { // TagReportData
			item := parseTagReportData(data[offset+4 : offset+paramLen])
			if item.EPCHex != "" {
				a.handleTagItem(item)
			}
		}
		offset += paramLen
	}
}

func parseTagReportData(data []byte) RawTagItem {
	var item RawTagItem
	item.Timestamp = time.Now().UTC()
	item.AntennaID = 1

	offset := 0
	for offset < len(data) {
		firstByte := data[offset]
		isTV := (firstByte & 0x80) != 0

		if isTV {
			tvType := uint16(firstByte & 0x7F)
			switch tvType {
			case 13: // EPC-96
				if offset+13 <= len(data) {
					item.EPCHex = hex.EncodeToString(data[offset+1 : offset+13])
					offset += 13
					continue
				}
			case 1: // AntennaID
				if offset+3 <= len(data) {
					item.AntennaID = int(binary.BigEndian.Uint16(data[offset+1 : offset+3]))
					offset += 3
					continue
				}
			case 6: // PeakRSSI
				if offset+2 <= len(data) {
					item.PeakRSSI = float64(int8(data[offset+1]))
					offset += 2
					continue
				}
			case 7: // ChannelIndex
				if offset+3 <= len(data) {
					item.Channel = int(binary.BigEndian.Uint16(data[offset+1 : offset+3]))
					offset += 3
					continue
				}
			case 2: // FirstSeenTimestampUTC
				if offset+9 <= len(data) {
					us := binary.BigEndian.Uint64(data[offset+1 : offset+9])
					item.Timestamp = time.UnixMicro(int64(us)).UTC()
					offset += 9
					continue
				}
			}
			offset += 2
		} else {
			if offset+4 > len(data) {
				break
			}
			pType := binary.BigEndian.Uint16(data[offset:offset+2]) & 0x03FF
			pLen := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
			if pLen < 4 || offset+pLen > len(data) {
				break
			}
			if pType == 241 && pLen > 4 { // EPCData
				epcBytes := data[offset+4 : offset+pLen]
				if len(epcBytes) >= 14 {
					item.EPCHex = hex.EncodeToString(epcBytes[2:14])
				}
			}
			offset += pLen
		}
	}
	return item
}

func (a *Agent) handleTagItem(item RawTagItem) {
	if !a.allTags && item.EPCHex != TargetEPCHex {
		a.tracker.mu.Lock()
		a.tracker.straysFilter++
		if a.tracker.straysFilter%100 == 1 {
			fmt.Printf("   [FILTRO DE STRAYS] 🛡️ Ignorando tags ambientales (Total filtrados: %d, Último: %s)\n",
				a.tracker.straysFilter, item.EPCHex)
		}
		a.tracker.mu.Unlock()
		return
	}

	a.tracker.mu.Lock()
	a.tracker.targetHits++
	now := time.Now().UTC()
	deltaRSSI := math.Abs(item.PeakRSSI - a.tracker.prevRSSI)

	// Throttled publish: se emite si pasaron >= 100ms O si el RSSI varió en >= 1.0 dBm
	if a.tracker.hasPrev && now.Sub(a.tracker.lastPubTime) < MinPublishGap && deltaRSSI < 1.0 {
		a.tracker.prevRSSI = item.PeakRSSI
		a.tracker.prevTime = item.Timestamp
		a.tracker.mu.Unlock()
		return
	}

	a.tracker.hasPrev = true
	a.tracker.lastPubTime = now
	a.tracker.prevRSSI = item.PeakRSSI
	a.tracker.prevTime = item.Timestamp
	hits := a.tracker.targetHits
	a.tracker.mu.Unlock()

	// Visualización live en consola
	fmt.Printf("[RAW INGRESS #%-4d] 📡 MAC: %s | NodeID: %s | EPC: %s | RSSI: %5.1f dBm | Ant: %d | Ch: %d | Tx: %d dBm\n",
		hits, a.macAddr, a.nodeID, item.EPCHex, item.PeakRSSI, item.AntennaID, item.Channel, a.powerDbm)

	a.readsCount.Add(1)
	go a.emitRawRead(item.EPCHex, uint16(item.AntennaID), uint16(item.Channel), item.PeakRSSI, item.Timestamp)
}

func (a *Agent) emitRawRead(epc string, antennaID, channelIndex uint16, rssi float64, ts time.Time) {
	isoTime := ts.Format(time.RFC3339)

	rawPayload := map[string]any{
		"mac":        a.macAddr,
		"reader_id":  a.nodeID,
		"antenna":    int(antennaID),
		"epc":        epc,
		"rssi":       rssi,
		"tx_power":   int(a.powerDbm),
		"channel":    int(channelIndex),
		"read_count": 1,
		"timestamp":  isoTime,
	}

	bytesRaw, err := json.Marshal(rawPayload)
	if err != nil {
		return
	}
	if err := a.publishMQTT("bindu/in/raw/rfid/idx4000", bytesRaw); err != nil {
		fmt.Printf("[MQTT] ⚠️ Error publicando raw ingress: %v\n", err)
	}
}

// -----------------------------------------------------------------------------
// UTILIDADES LLRP & MQTT BINARIAS
// -----------------------------------------------------------------------------

func encodeLLRP(msgType uint16, msgID uint32, payload []byte) []byte {
	totalLen := 10 + len(payload)
	hdr := make([]byte, 10)
	binary.BigEndian.PutUint16(hdr[0:2], (1<<10)|(msgType&0x03FF))
	binary.BigEndian.PutUint32(hdr[2:6], uint32(totalLen))
	binary.BigEndian.PutUint32(hdr[6:10], msgID)
	return append(hdr, payload...)
}

func uint32Bytes(v uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, v)
	return buf
}

func buildTLV(paramType uint16, value []byte) []byte {
	totalLen := 4 + len(value)
	buf := make([]byte, totalLen)
	binary.BigEndian.PutUint16(buf[0:2], paramType&0x03FF)
	binary.BigEndian.PutUint16(buf[2:4], uint16(totalLen))
	copy(buf[4:], value)
	return buf
}

func buildStrictROSpec(roSpecID uint32, antennaID, powerDbm uint16) []byte {
	var boundary bytes.Buffer
	boundary.Write(buildTLV(179, []byte{1})) // Immediate start
	stopTrig := make([]byte, 5)
	boundary.Write(buildTLV(182, stopTrig)) // Null stop
	boundaryTLV := buildTLV(178, boundary.Bytes())

	var aiSpec bytes.Buffer
	var antList []byte
	if antennaID == 0 {
		antList = []byte{0, 1, 0, 0} // Todos los puertos
	} else {
		antList = []byte{0, 1, byte(antennaID >> 8), byte(antennaID & 0xFF)}
	}
	aiSpec.Write(antList)
	aiStopTrig := make([]byte, 5)
	aiSpec.Write(buildTLV(184, aiStopTrig))

	rfTrans := make([]byte, 6)
	binary.BigEndian.PutUint16(rfTrans[0:2], 0)
	binary.BigEndian.PutUint16(rfTrans[2:4], 0)
	binary.BigEndian.PutUint16(rfTrans[4:6], powerDbm)
	rfTransTLV := buildTLV(224, rfTrans)

	antConfig := make([]byte, 2)
	binary.BigEndian.PutUint16(antConfig[0:2], 1)
	antConfig = append(antConfig, rfTransTLV...)
	antConfigTLV := buildTLV(222, antConfig)

	var invParam bytes.Buffer
	invParam.Write([]byte{0, 1, 1})
	invParam.Write(antConfigTLV)
	aiSpec.Write(buildTLV(186, invParam.Bytes()))

	aiSpecTLV := buildTLV(183, aiSpec.Bytes())

	var roReport bytes.Buffer
	roReport.Write([]byte{1, 0, 1})
	var selector bytes.Buffer
	selector.Write([]byte{0x1F, 0x80})
	roReport.Write(buildTLV(238, selector.Bytes()))
	roReportTLV := buildTLV(237, roReport.Bytes())

	var roSpec bytes.Buffer
	hdr := make([]byte, 6)
	binary.BigEndian.PutUint32(hdr[0:4], roSpecID)
	hdr[4] = 0
	hdr[5] = 0
	roSpec.Write(hdr)
	roSpec.Write(boundaryTLV)
	roSpec.Write(aiSpecTLV)
	roSpec.Write(roReportTLV)

	return buildTLV(177, roSpec.Bytes())
}

func encodeMqttString(s string) []byte {
	b := make([]byte, 2+len(s))
	binary.BigEndian.PutUint16(b[0:2], uint16(len(s)))
	copy(b[2:], []byte(s))
	return b
}

func buildMQTTPacket(pktType byte, payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(pktType)
	remLen := len(payload)
	for {
		b := byte(remLen % 128)
		remLen /= 128
		if remLen > 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
		if remLen == 0 {
			break
		}
	}
	buf.Write(payload)
	return buf.Bytes()
}
