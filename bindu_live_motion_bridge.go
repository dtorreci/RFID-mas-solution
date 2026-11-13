package main

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	ReaderAddr   = "192.168.1.86:5084"
	TargetGTIN   = "07621806175810"
	MqttBroker   = "127.0.0.1:1883"
	MqttUser     = ""
	MqttPass     = ""
	MqttTopic    = "bindu/in"
	MinReportGap = 150 * time.Millisecond // Límite de cadencia para ingesta fluida
)

type Bridge struct {
	readerConn net.Conn
	mqttConn   net.Conn
	mu         sync.Mutex
	lastSent   time.Time
	sentCount  int
	strayCount int
}

func main() {
	fmt.Println("=========================================================================================")
	fmt.Printf("   🌉 PASARELA EN VIVO RFID LLRP ➔ BINDU MAS (mTLS + CEL Reactor)\n")
	fmt.Printf("   📡 Lector R2000: %s | Antena: Keonn Advan-CP1 EU (Puerto 1)\n", ReaderAddr)
	fmt.Printf("   🎯 Tag de Interés: GTIN %s (Filtro estricto de strays)\n", TargetGTIN)
	fmt.Printf("   ⚡ Motor Reactivo: CEL Reactor 'keonn-advan-cp1-sgtin-radial-motion-tracker'\n")
	fmt.Println("=========================================================================================")

	bridge := &Bridge{}

	// 1. Conexión MQTT mTLS a Bindu
	fmt.Print("1. Conectando al Broker MQTT de Bindu (127.0.0.1:1883 mTLS)... ")
	if err := bridge.connectMQTT(); err != nil {
		fmt.Printf("\n❌ Error conectando a Bindu MQTT: %v\n", err)
		return
	}
	defer bridge.mqttConn.Close()
	fmt.Println("✔ Conectado y autenticado.")

	// 2. Conexión LLRP al lector R2000
	fmt.Print("2. Conectando al lector R2000 (192.168.1.86:5084 LLRP)... ")
	rConn, err := net.DialTimeout("tcp", ReaderAddr, 5*time.Second)
	if err != nil {
		fmt.Printf("\n❌ Error conectando a lector RFID: %v\n", err)
		return
	}
	bridge.readerConn = rConn
	defer bridge.readerConn.Close()

	if err := bridge.initLLRP(); err != nil {
		fmt.Printf("\n❌ Error configurando LLRP ROSpec: %v\n", err)
		return
	}
	fmt.Println("✔ ROSpec activo en Antena 1 (Keonn Advan-CP1).")
	fmt.Println("=========================================================================================")
	fmt.Println("✔ LEYENDO EN VIVO... Mueve el tag frente a la antena (Presiona Ctrl+C para finalizar)")
	fmt.Println("-----------------------------------------------------------------------------------------")
	fmt.Printf("%-10s | %-16s | %-9s | %-12s | %-18s | %s\n", "Hora", "GTIN", "RSSI", "Dist. Estimada", "Zona de Antena", "Pipeline Bindu")
	fmt.Println("-----------+------------------+-----------+--------------+--------------------+--------------------------")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go bridge.runLLRPLoop()

	<-sigChan
	fmt.Println("\nDeteniendo ROSpec...")
	bridge.stopLLRP()
	fmt.Printf("✔ Sesión finalizada. Total eventos inyectados a Bindu: %d | Strays ignorados: %d\n", bridge.sentCount, bridge.strayCount)
}

func (b *Bridge) connectMQTT() error {
	cert, err := tls.LoadX509KeyPair("/home/dtp/.bindu/root/runtime/tls/root.crt", "/home/dtp/.bindu/root/runtime/tls/root.key")
	if err != nil {
		return fmt.Errorf("error cargando certs mTLS: %w", err)
	}
	tlsConf := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
	}
	conn, err := tls.Dial("tcp", MqttBroker, tlsConf)
	if err != nil {
		return fmt.Errorf("error dial mTLS: %w", err)
	}

	var varHdr []byte
	varHdr = append(varHdr, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04)
	connectFlags := byte(0x02)
	if MqttUser != "" {
		connectFlags |= 0x80
	}
	if MqttPass != "" {
		connectFlags |= 0x40
	}
	varHdr = append(varHdr, connectFlags, 0x00, 0x3C)

	var payload []byte
	payload = append(payload, encodeString("rfid-live-motion-bridge")...)
	if MqttUser != "" {
		payload = append(payload, encodeString(MqttUser)...)
	}
	if MqttPass != "" {
		payload = append(payload, encodeString(MqttPass)...)
	}

	conn.Write(buildMQTTPacket(0x10, append(varHdr, payload...)))
	ack := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(ack); err != nil {
		conn.Close()
		return fmt.Errorf("timeout leyendo CONNACK: %w", err)
	}
	conn.SetReadDeadline(time.Time{})
	if ack[0] != 0x20 || ack[3] != 0x00 {
		conn.Close()
		return fmt.Errorf("CONNACK rechazado (código %d)", ack[3])
	}

	b.mqttConn = conn
	return nil
}

func (b *Bridge) initLLRP() error {
	hdr := make([]byte, 10)
	io.ReadFull(b.readerConn, hdr)
	l := binary.BigEndian.Uint32(hdr[2:6])
	body := make([]byte, int(l)-10)
	io.ReadFull(b.readerConn, body)

	// DELETE_ROSPEC 1
	b.readerConn.Write(encodeLLRP(21, 101, uint32Bytes(1)))
	io.ReadFull(b.readerConn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(b.readerConn, body)

	// ADD_ROSPEC Antena 1
	b.readerConn.Write(encodeLLRP(20, 102, buildStrictROSpec(1)))
	io.ReadFull(b.readerConn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(b.readerConn, body)

	// ENABLE_ROSPEC 1
	b.readerConn.Write(encodeLLRP(24, 103, uint32Bytes(1)))
	io.ReadFull(b.readerConn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(b.readerConn, body)

	// START_ROSPEC 1
	b.readerConn.Write(encodeLLRP(22, 104, uint32Bytes(1)))
	io.ReadFull(b.readerConn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(b.readerConn, body)

	return nil
}

func (b *Bridge) stopLLRP() {
	if b.readerConn != nil {
		b.readerConn.SetDeadline(time.Now().Add(2 * time.Second))
		b.readerConn.Write(encodeLLRP(23, 105, uint32Bytes(1)))
		hdr := make([]byte, 10)
		io.ReadFull(b.readerConn, hdr)
		l := binary.BigEndian.Uint32(hdr[2:6])
		body := make([]byte, int(l)-10)
		io.ReadFull(b.readerConn, body)
	}
}

func (b *Bridge) runLLRPLoop() {
	hdr := make([]byte, 10)
	for {
		b.readerConn.SetDeadline(time.Now().Add(500 * time.Millisecond))
		if _, err := io.ReadFull(b.readerConn, hdr); err != nil {
			continue
		}
		msgType := binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
		msgLen := binary.BigEndian.Uint32(hdr[2:6])
		bodyLen := int(msgLen) - 10
		if bodyLen < 0 || bodyLen > 10*1024*1024 {
			continue
		}
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(b.readerConn, body); err != nil {
			continue
		}

		if msgType == 61 { // RO_ACCESS_REPORT (0x3D)
			items := parseReportPayload(body)
			for _, item := range items {
				if item.EPCHex == "" {
					continue
				}
				gtin := item.EPCHex
				serial := ""
				standard := "UNKNOWN"
				if res, err := DecodeEPC(item.EPCHex); err == nil && res.GTIN14 != "" {
					gtin = res.GTIN14
					serial = res.SerialNumber
					standard = res.Standard
				}

				if gtin == TargetGTIN || item.EPCHex == TargetGTIN {
					b.publishToBindu(item.EPCHex, gtin, serial, standard, item.AntennaID, item.Channel, item.PeakRSSI)
				} else {
					b.mu.Lock()
					b.strayCount++
					b.mu.Unlock()
				}
			}
		} else if msgType == 62 { // KEEPALIVE
			ack := encodeLLRP(72, binary.BigEndian.Uint32(hdr[6:10]), nil)
			b.readerConn.Write(ack)
		}
	}
}

func (b *Bridge) publishToBindu(epcHex, gtin, serial, standard string, antennaID, channelIdx int, rssi float64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	if now.Sub(b.lastSent) < MinReportGap {
		return
	}
	b.lastSent = now
	b.sentCount++

	nowUTC := now.UTC().Format(time.RFC3339)
	companyPrefix := "7621806"
	itemReference := "017581"
	canonicalURN := fmt.Sprintf("urn:epc:tag:sgtin-96:5.%s.%s.%s", companyPrefix, itemReference, serial)

	eventJSON := fmt.Sprintf(`{
		"$type": "EpcSgtinRead",
		"$group": "mesh",
		"$node-dest": "root",
		"$producer": "rfid-bridge",
		"$user": "dtorreci",
		"$when": "%s",
		"fields": {
			"epc_hex": "%s",
			"standard": "%s",
			"company_prefix": "%s",
			"item_reference": "%s",
			"antenna_id": %d,
			"channel_index": %d,
			"peak_rssi_dbm": %.1f,
			"gtin14": "%s",
			"serial_number": "%s",
			"reader_id": "portal-reader-01",
			"canonical_urn": "%s",
			"first_seen_utc": "%s",
			"last_seen_utc": "%s"
		}
	}`, nowUTC, epcHex, standard, companyPrefix, itemReference, antennaID, channelIdx, rssi, gtin, serial, canonicalURN, nowUTC, nowUTC)

	var pubVarHdr []byte
	pubVarHdr = append(pubVarHdr, encodeString(MqttTopic)...)
	pubPkt := buildMQTTPacket(0x30, append(pubVarHdr, []byte(eventJSON)...))
	if _, err := b.mqttConn.Write(pubPkt); err != nil {
		fmt.Printf("❌ Error publicando MQTT: %v\n", err)
		return
	}

	distM, marginM, zone := estimateContactDistance(rssi)
	distStr := fmt.Sprintf("%.2fm±%.0fcm", distM, marginM*100)

	fmt.Printf("%-10s | %-16s | %5.1f dBm | %-12s | %-18s | ⚡ EpcSgtinRead ➔ CEL\n",
		now.Format("15:04:05.0"), gtin, rssi, distStr, zone)
}

func estimateContactDistance(rssi float64) (float64, float64, string) {
	if rssi >= 73.0 {
		return 0.01, 0.01, "CONTACTO (Radomo)"
	} else if rssi >= 68.0 {
		return 0.01 + (73.0-rssi)*0.014, 0.02, "PROXIMIDAD (Cerca)"
	} else if rssi >= 60.0 {
		return 0.08 + (68.0-rssi)*0.025, 0.05, "CAMPO CERCANO"
	} else if rssi >= 52.0 {
		return 0.28 + (60.0-rssi)*0.045, 0.10, "TRÁNSITO MEDIO"
	} else if rssi >= 45.0 {
		return 0.64 + (52.0-rssi)*0.080, 0.20, "TRÁNSITO MEDIO"
	}
	return 1.20 + (45.0-rssi)*0.120, 0.35, "CAMPO LEJANO"
}

type RawTagItem struct {
	Timestamp time.Time
	AntennaID int
	PeakRSSI  float64
	Channel   int
	EPCHex    string
}

func parseReportPayload(data []byte) []RawTagItem {
	var items []RawTagItem
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
				items = append(items, item)
			}
		}
		offset += paramLen
	}
	return items
}

func parseTagReportData(data []byte) RawTagItem {
	item := RawTagItem{
		Timestamp: time.Now().UTC(),
		AntennaID: 1,
		PeakRSSI:  -60.0,
		Channel:   1,
	}

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

func buildStrictROSpec(roSpecID uint32) []byte {
	var boundary bytes.Buffer
	boundary.Write(buildTLV(179, []byte{1})) // Immediate start
	stopTrig := make([]byte, 5)
	boundary.Write(buildTLV(182, stopTrig)) // Null stop
	boundaryTLV := buildTLV(178, boundary.Bytes())

	var aiSpec bytes.Buffer
	antList := []byte{0, 1, 0, 1} // Exclusivo Antena 1 (Keonn Advan-CP1)
	aiSpec.Write(antList)
	aiStopTrig := make([]byte, 5)
	aiSpec.Write(buildTLV(184, aiStopTrig))
	invParam := []byte{0, 1, 1}
	aiSpec.Write(buildTLV(186, invParam))
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

func encodeLLRP(msgType uint16, msgID uint32, payload []byte) []byte {
	totalLen := 10 + len(payload)
	buf := make([]byte, totalLen)
	firstWord := (uint16(1) << 10) | (msgType & 0x03FF)
	binary.BigEndian.PutUint16(buf[0:2], firstWord)
	binary.BigEndian.PutUint32(buf[2:6], uint32(totalLen))
	binary.BigEndian.PutUint32(buf[6:10], msgID)
	copy(buf[10:], payload)
	return buf
}

func encodeString(s string) []byte {
	buf := make([]byte, 2+len(s))
	binary.BigEndian.PutUint16(buf[0:2], uint16(len(s)))
	copy(buf[2:], []byte(s))
	return buf
}

func buildMQTTPacket(pktType byte, data []byte) []byte {
	var remLen []byte
	x := len(data)
	for {
		encodedByte := byte(x % 128)
		x = x / 128
		if x > 0 {
			encodedByte |= 128
		}
		remLen = append(remLen, encodedByte)
		if x == 0 {
			break
		}
	}
	var pkt []byte
	pkt = append(pkt, pktType)
	pkt = append(pkt, remLen...)
	pkt = append(pkt, data...)
	return pkt
}
