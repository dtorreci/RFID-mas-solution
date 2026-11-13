package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"
)

type DecodedTagEvent struct {
	Timestamp time.Time
	AntennaID int
	RSSI      float64
	Channel   int
	EPCHex    string
	Standard  string
	URN       string
	Details   string
	Count     int
}

func main() {
	target := "192.168.1.86:5084"
	fmt.Printf("=== Iniciando Ingesta en Vivo desde Lector R2000 (%s) ===\n", target)

	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer conn.Close()

	// 1. Skip init notification
	hdr := make([]byte, 10)
	io.ReadFull(conn, hdr)
	l := binary.BigEndian.Uint32(hdr[2:6])
	body := make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	// 2. Delete existing ROSpec 1
	conn.Write(encodeLLRP(21, 101, uint32Bytes(1)))
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	// 3. Add strict valid ROSpec
	addPayload := buildStrictROSpec(1)
	conn.Write(encodeLLRP(20, 102, addPayload))
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	// 4. Enable ROSpec
	conn.Write(encodeLLRP(24, 103, uint32Bytes(1)))
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	// 5. Start ROSpec
	conn.Write(encodeLLRP(22, 104, uint32Bytes(1)))
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	fmt.Println("✔ Lector activado y emitiendo campo RF en las 4 antenas.")
	fmt.Println("Capturando tags durante 6 segundos...\n")

	var mu sync.Mutex
	tagMap := make(map[string]*DecodedTagEvent)
	totalReports := 0

	_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			break
		}
		msgType := binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
		msgLen := binary.BigEndian.Uint32(hdr[2:6])
		payload := make([]byte, int(msgLen)-10)
		if _, err := io.ReadFull(conn, payload); err != nil {
			break
		}

		if msgType == 61 { // RO_ACCESS_REPORT
			totalReports++
			items := parseReportPayload(payload)
			for _, item := range items {
				if item.EPCHex == "" {
					continue
				}
				mu.Lock()
				existing, ok := tagMap[item.EPCHex]
				if !ok {
					// Decodificar con BitReader
					std := "Unknown"
					urn := "urn:epc:raw:" + item.EPCHex
					details := ""
					if res, err := DecodeEPC(item.EPCHex); err == nil {
						std = res.Standard
						urn = res.CanonicalURN
						switch res.Standard {
						case "SGTIN-96":
							details = fmt.Sprintf("GTIN: %s, Serial: %s", res.GTIN14, res.SerialNumber)
						case "SSCC-96":
							details = fmt.Sprintf("SSCC: %s", res.SSCC18)
						case "SGLN-96":
							details = fmt.Sprintf("GLN: %s", res.GLN13)
						case "GRAI-96":
							details = fmt.Sprintf("Asset: %s, Serial: %s", res.AssetType, res.SerialNumber)
						default:
							details = fmt.Sprintf("Prefix: %s", res.CompanyPrefix)
						}
					}
					tagMap[item.EPCHex] = &DecodedTagEvent{
						Timestamp: item.Timestamp,
						AntennaID: item.AntennaID,
						RSSI:      item.PeakRSSI,
						Channel:   item.Channel,
						EPCHex:    item.EPCHex,
						Standard:  std,
						URN:       urn,
						Details:   details,
						Count:     1,
					}
					fmt.Printf("[NUEVO TAG DETECTADO] Antena %d | RSSI: %.1f dBm | Canal: %d | EPC: %s\n",
						item.AntennaID, item.PeakRSSI, item.Channel, item.EPCHex)
					if std != "Unknown" {
						fmt.Printf("  ↳ Estándar GS1: %s | %s\n", std, details)
						fmt.Printf("  ↳ URN: %s\n\n", urn)
					}
				} else {
					existing.Count++
					if item.PeakRSSI > existing.RSSI {
						existing.RSSI = item.PeakRSSI
						existing.AntennaID = item.AntennaID
					}
				}
				mu.Unlock()
			}
		} else if msgType == 62 { // KEEPALIVE
			ack := encodeLLRP(72, binary.BigEndian.Uint32(hdr[6:10]), nil)
			conn.Write(ack)
		}
	}

	// 6. Stop ROSpec
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	conn.Write(encodeLLRP(23, 105, uint32Bytes(1)))
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)
	fmt.Println("\n✔ Lectura finalizada y campo RF detenido.")

	// Resumen final
	fmt.Println("\n=========================================================================================")
	fmt.Printf("                     RESUMEN DE TAGS LEÍDOS EN VIVO (Total reportes: %d)\n", totalReports)
	fmt.Println("=========================================================================================")
	fmt.Printf("%-4s | %-7s | %-8s | %-10s | %-26s | %-25s\n", "Ant", "RSSI", "Lecturas", "Estándar", "EPC (Hex)", "Detalle GS1")
	fmt.Println("-----+---------+----------+------------+----------------------------+--------------------")

	var sortedKeys []string
	for k := range tagMap {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	for _, k := range sortedKeys {
		t := tagMap[k]
		fmt.Printf("A%-3d | %5.1f   | %-8d | %-10s | %-26s | %s\n",
			t.AntennaID, t.RSSI, t.Count, t.Standard, t.EPCHex, t.Details)
	}
	fmt.Println("=========================================================================================")
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
			if pType == 241 && pLen > 4 { // EPCData (variable length)
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
	antList := []byte{0, 1, 0, 0} // All antennas
	aiSpec.Write(antList)
	aiStopTrig := make([]byte, 5) // Null stop
	aiSpec.Write(buildTLV(184, aiStopTrig))
	invParam := []byte{0, 1, 1} // SpecID=1, Gen2
	aiSpec.Write(buildTLV(186, invParam))
	aiSpecTLV := buildTLV(183, aiSpec.Bytes())

	var roReport bytes.Buffer
	roReport.Write([]byte{1, 0, 1}) // Trigger=Upon_N_Tags, N=1
	var selector bytes.Buffer
	selector.Write([]byte{0x1F, 0x80}) // Enable Antenna, Channel, Timestamps, RSSI, SeenCount
	roReport.Write(buildTLV(238, selector.Bytes()))
	roReportTLV := buildTLV(237, roReport.Bytes())

	var roSpec bytes.Buffer
	hdr := make([]byte, 6)
	binary.BigEndian.PutUint32(hdr[0:4], roSpecID)
	hdr[4] = 0 // Priority = 0
	hdr[5] = 0 // CurrentState = Disabled (0)
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
