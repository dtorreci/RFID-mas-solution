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
	"strings"
	"sync"
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
	DefaultMQTTAddr = "192.168.1.75:1883"
	TargetGTIN      = "07621806175810"
	TargetEPCHex    = "30b5d132b8112b42f9b59b2a"
	TargetSerial    = "12779363114"
	TargetPrefix    = "7621806"
	TargetItemRef   = "017581"
	AntennaModel    = "Keonn Advan-CP1 EU"
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

type Agent struct {
	macAddr   string
	readerID  string
	llrpAddr  string
	mqttAddr  string
	antennaID uint16
	powerDbm  uint16
	allTags   bool
	mqttUser  string
	mqttPass  string
	llrpConn  net.Conn
	mqttConn  net.Conn
	tracker   *MotionTracker
	running   bool
	mqttMu    sync.Mutex
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

func main() {
	llrpAddr := flag.String("llrp", DefaultLLRPAddr, "LLRP reader address (host:port)")
	mqttAddr := flag.String("mqtt", DefaultMQTTAddr, "Bindu MQTT broker address (host:port)")
	user := flag.String("user", "", "MQTT username")
	pass := flag.String("pass", "", "MQTT password")
	ant := flag.Int("ant", 0, "Antenna port (0=all antennas, 1-4)")
	power := flag.Int("power", 18, "Transmit power in dBm (default: 18 dBm)")
	allTags := flag.Bool("all-tags", true, "Publish all tags (false=filter target EPC only)")
	flag.Parse()

	mac := getMacAddress()
	cleanMac := strings.ToLower(strings.ReplaceAll(mac, ":", ""))
	shortID := cleanMac
	if len(shortID) > 6 {
		shortID = shortID[len(shortID)-6:]
	}
	readerID := fmt.Sprintf("idx4000-%s", shortID)

	fmt.Println("=========================================================================================")
	fmt.Printf("   🚀 SENSORMATIC IDX-4000 ON-READER EDGE AGENT (ARMv7 Native)\n")
	fmt.Printf("   🏷️ Hardware MAC: %s | Reader ID: %s\n", mac, readerID)
	fmt.Printf("   📡 Local LLRP: %s | Antena Port %d (%s) | Potencia: %d dBm\n", *llrpAddr, *ant, AntennaModel, *power)
	fmt.Printf("   🎯 Target GTIN: %s (EPC: %s)\n", TargetGTIN, TargetEPCHex)
	fmt.Printf("   📦 Raw Ingress Pipeline: bindu/in/raw/rfid/idx4000 (Pure decoupled telemetry)\n")
	fmt.Printf("   🔒 MQTT Broker: %s (mTLS with Embedded Root CA + Client Cert)\n", *mqttAddr)
	fmt.Println("=========================================================================================")

	agent := &Agent{
		macAddr:   mac,
		readerID:  readerID,
		llrpAddr:  *llrpAddr,
		mqttAddr:  *mqttAddr,
		antennaID: uint16(*ant),
		powerDbm:  uint16(*power),
		allTags:   *allTags,
		mqttUser:  *user,
		mqttPass:  *pass,
		tracker:   &MotionTracker{},
		running:   true,
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
	if a.llrpConn != nil {
		a.llrpConn.Write(encodeLLRP(23, 999, uint32Bytes(1))) // STOP_ROSPEC
		a.llrpConn.Close()
	}
	a.disconnectMQTT()
}

func (a *Agent) run() error {
	for a.running {
		fmt.Printf("\n[CONEXIÓN] Estableciendo enlace mTLS con Bindu Mesh Gateway (%s)...\n", a.mqttAddr)
		if err := a.connectMQTT(); err != nil {
			fmt.Printf("[MQTT] ⚠️ Fallo al conectar con Bindu: %v. Reintentando en 3s...\n", err)
			time.Sleep(3 * time.Second)
			continue
		}
		fmt.Printf("[MQTT] ✅ Enlace mTLS autenticado exitosamente (Client: %s, User: %s)\n", a.readerID, a.mqttUser)

		// Goroutine de ping periódico para mantener vivo el socket MQTT
		stopPing := make(chan struct{})
		go func() {
			ticker := time.NewTicker(25 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					a.mqttMu.Lock()
					if a.mqttConn != nil {
						a.mqttConn.Write([]byte{0xC0, 0x00}) // PINGREQ
					}
					a.mqttMu.Unlock()
				case <-stopPing:
					return
				}
			}
		}()

		fmt.Printf("[CONEXIÓN] Conectando a servicio interno LLRP (%s)...\n", a.llrpAddr)
		if err := a.connectLLRP(); err != nil {
			fmt.Printf("[LLRP] ⚠️ Error conectando con ireader daemon: %v. Reintentando en 3s...\n", err)
			close(stopPing)
			a.disconnectMQTT()
			time.Sleep(3 * time.Second)
			continue
		}
		fmt.Println("[LLRP] ✅ Conexión establecida con servicio RFID local de Sensormatic.")

		fmt.Printf("[ROSPEC] Configurando ROSpec #1 (Antena %d a %d dBm)...\n", a.antennaID, a.powerDbm)
		if err := a.setupAndStartROSpec(); err != nil {
			fmt.Printf("[ROSPEC] ⚠️ Fallo al inicializar ROSpec: %v\n", err)
			close(stopPing)
			a.disconnectLLRP()
			a.disconnectMQTT()
			time.Sleep(3 * time.Second)
			continue
		}
		fmt.Println("[ROSPEC] ✅ ROSpec ejecutándose. Lector radiando en puerto de antena 1.")

		if err := a.readLoop(); err != nil {
			fmt.Printf("[LOOP] Desconexión de datos: %v\n", err)
		}

		close(stopPing)
		a.disconnectLLRP()
		a.disconnectMQTT()

		if a.running {
			fmt.Println("[RECONNECT] Esperando 3 segundos antes de reiniciar ciclo...")
			time.Sleep(3 * time.Second)
		}
	}
	return nil
}

func (a *Agent) connectMQTT() error {
	a.mqttMu.Lock()
	defer a.mqttMu.Unlock()

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCertPEM) {
		return fmt.Errorf("fallo al cargar ca.crt embebido")
	}

	cert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		return fmt.Errorf("fallo al cargar par de claves mTLS: %w", err)
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
	payload = append(payload, encodeMqttString(a.readerID)...)
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
	return err
}

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
	t := binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
	fmt.Printf("[LLRP] DELETE_ROSPEC resp type=%d, len=%d\n", t, l)

	// ADD_ROSPEC 1
	a.llrpConn.Write(encodeLLRP(20, 102, buildStrictROSpec(1, a.antennaID, a.powerDbm)))
	io.ReadFull(a.llrpConn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(a.llrpConn, body)
	t = binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
	fmt.Printf("[LLRP] ADD_ROSPEC resp type=%d, len=%d\n", t, l)

	// ENABLE_ROSPEC 1
	a.llrpConn.Write(encodeLLRP(24, 103, uint32Bytes(1)))
	io.ReadFull(a.llrpConn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(a.llrpConn, body)
	t = binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
	fmt.Printf("[LLRP] ENABLE_ROSPEC resp type=%d, len=%d\n", t, l)

	// START_ROSPEC 1
	a.llrpConn.Write(encodeLLRP(22, 104, uint32Bytes(1)))
	io.ReadFull(a.llrpConn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(a.llrpConn, body)
	t = binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
	fmt.Printf("[LLRP] START_ROSPEC resp type=%d, len=%d\n", t, l)

	return nil
}

func (a *Agent) readLoop() error {
	hdr := make([]byte, 10)
	for a.running {
		if _, err := io.ReadFull(a.llrpConn, hdr); err != nil {
			return err
		}

		msgType := (binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF)
		msgLen := binary.BigEndian.Uint32(hdr[2:6])
		if msgLen < 10 {
			return fmt.Errorf("longitud de mensaje inválida: %d", msgLen)
		}

		body := make([]byte, int(msgLen)-10)
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
	fmt.Printf("[RAW INGRESS #%-4d] 📡 MAC: %s | ReaderID: %s | EPC: %s | RSSI: %5.1f dBm | Ant: %d | Ch: %d | Tx: %d dBm\n",
		hits, a.macAddr, a.readerID, item.EPCHex, item.PeakRSSI, item.AntennaID, item.Channel, a.powerDbm)

	go a.emitRawRead(item.EPCHex, uint16(item.AntennaID), uint16(item.Channel), item.PeakRSSI, item.Timestamp)
}

func (a *Agent) emitRawRead(epc string, antennaID, channelIndex uint16, rssi float64, ts time.Time) {
	isoTime := ts.Format(time.RFC3339)

	rawPayload := map[string]any{
		"mac":        a.macAddr,
		"reader_id":  a.readerID,
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
	// Si antennaID == 0 se activan todos los puertos; si es > 0, el puerto específico
	var antList []byte
	if antennaID == 0 {
		antList = []byte{0, 1, 0, 0} // Todos los puertos de antena (1, 2, 3, 4)
	} else {
		antList = []byte{0, 1, byte(antennaID >> 8), byte(antennaID & 0xFF)}
	}
	aiSpec.Write(antList)
	aiStopTrig := make([]byte, 5)
	aiSpec.Write(buildTLV(184, aiStopTrig))

	// RFTransmitter (Type 224): HopTableID=0, ChannelIndex=0, TransmitPower=powerDbm
	rfTrans := make([]byte, 6)
	binary.BigEndian.PutUint16(rfTrans[0:2], 0)
	binary.BigEndian.PutUint16(rfTrans[2:4], 0)
	binary.BigEndian.PutUint16(rfTrans[4:6], powerDbm)
	rfTransTLV := buildTLV(224, rfTrans)

	// AntennaConfiguration (Type 222): AntennaID=1 + RFTransmitter
	antConfig := make([]byte, 2)
	binary.BigEndian.PutUint16(antConfig[0:2], 1)
	antConfig = append(antConfig, rfTransTLV...)
	antConfigTLV := buildTLV(222, antConfig)

	// InventoryParameterSpec (Type 186): SpecID=1, Protocol=1 (EPC Class 1 Gen 2) + AntennaConfiguration
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
