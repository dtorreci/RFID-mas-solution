package main

import (
	"bytes"
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

const TargetGTIN = "07621806175810"

type RSSISample struct {
	Timestamp time.Time
	RSSI      float64
}

type TagMotionTracker struct {
	EPCHex       string
	GTIN         string
	IsTarget     bool
	RawRSSI      float64
	SmoothRSSI   float64
	History      []RSSISample
	LastReadTime time.Time
	TotalReads   int
	CurrentState string
	Delta1s      float64
	Delta300ms   float64
}

const (
	EmaAlpha       = 0.35 // Filtro EMA contra saltos de canal
	MotionThreshDB = 1.5  // 1.5 dB de cambio continuo para confirmar movimiento
)

func main() {
	target := "192.168.1.86:5084"
	fmt.Println("=========================================================================================")
	fmt.Printf("   📡 RASTREADOR CINEMÁTICO EN VIVO — Antena Keonn Advan-CP1 EU (%s)\n", target)
	fmt.Printf("   🎯 TAG OBJETIVO: %s (Filtro activo de strays)\n", TargetGTIN)
	fmt.Println("=========================================================================================")
	fmt.Println("Conectando al lector R2000 (LLRP)...")

	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		fmt.Printf("Error conectando a %s: %v\n", target, err)
		return
	}
	defer conn.Close()

	// 1. Handshake LLRP inicial
	hdr := make([]byte, 10)
	io.ReadFull(conn, hdr)
	l := binary.BigEndian.Uint32(hdr[2:6])
	body := make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	// 2. Limpiar ROSpecs
	conn.Write(encodeLLRP(21, 101, uint32Bytes(1)))
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	// 3. Agregar ROSpec Antena 1 (Keonn Advan-CP1)
	conn.Write(encodeLLRP(20, 102, buildStrictROSpec(1)))
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	// 4. Activar e Iniciar ROSpec
	conn.Write(encodeLLRP(24, 103, uint32Bytes(1)))
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	conn.Write(encodeLLRP(22, 104, uint32Bytes(1)))
	io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	body = make([]byte, int(l)-10)
	io.ReadFull(conn, body)

	fmt.Println("✔ Lector emitiendo en Antena 1 (Keonn Advan-CP1).")
	fmt.Println("✔ ¡MUEVE EL TAG OBJETIVO AHORA! (Acércalo hacia la antena y luego aléjalo)")
	fmt.Println("Duración de la captura: 50 segundos o Ctrl+C...")
	fmt.Printf("%-10s | %-16s | %-9s | %-8s | %-8s | %-10s | %-17s | %s\n",
		"Hora", "Tag / GTIN", "RSSI Inst", "RSSI EMA", "Δ (1s)", "Distancia", "Estado Cinemático", "Vector de Movimiento")
	fmt.Println("-----------+------------------+-----------+----------+----------+------------+-------------------+-----------------------------")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	var mu sync.Mutex
	trackers := make(map[string]*TagMotionTracker)
	strayReadsCount := 0
	done := make(chan struct{})

	go func() {
		select {
		case <-sigChan:
			close(done)
		case <-time.After(50 * time.Second):
			close(done)
		}
	}()

	// Refresco en consola cada 200ms
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				for _, tr := range trackers {
					if !tr.IsTarget {
						continue
					}
					if time.Since(tr.LastReadTime) < 500*time.Millisecond {
						state := tr.CurrentState
						var vector string
						switch state {
						case "🟢 ACERCÁNDOSE":
							vector = fmt.Sprintf("▲▲▲▲▲ [Δ1s=%+4.1fdB] >>> APROXIMACIÓN", tr.Delta1s)
						case "🔴 ALEJÁNDOSE":
							vector = fmt.Sprintf("▼▼▼▼▼ [Δ1s=%+4.1fdB] <<< RETICENCIA/ALEJAMIENTO", tr.Delta1s)
						default:
							vector = fmt.Sprintf("───── [Δ1s=%+4.1fdB] --- ESTÁTICO", tr.Delta1s)
						}

						distM, marginM, _ := estimateDistance(tr.SmoothRSSI)
						distStr := fmt.Sprintf("%.2fm±%.0fcm", distM, marginM*100)

						fmt.Printf("%-10s | %-16s | %5.1f dBm | %5.1f dBm | %+5.1f dB | %-10s | %-17s | %s\n",
							time.Now().Format("15:04:05.0"), "🎯 "+tr.GTIN, tr.RawRSSI, tr.SmoothRSSI, tr.Delta1s, distStr, state, vector)
					}
				}
				mu.Unlock()
			case <-done:
				return
			}
		}
	}()

	// Ingesta continua LLRP
	for {
		select {
		case <-done:
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			conn.Write(encodeLLRP(23, 105, uint32Bytes(1)))
			io.ReadFull(conn, hdr)
			l = binary.BigEndian.Uint32(hdr[2:6])
			body = make([]byte, int(l)-10)
			io.ReadFull(conn, body)
			fmt.Printf("\n✔ Captura finalizada. Total strays filtrados: %d.\n", strayReadsCount)
			return
		default:
		}

		conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
		if _, err := io.ReadFull(conn, hdr); err != nil {
			continue
		}
		msgType := binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
		msgLen := binary.BigEndian.Uint32(hdr[2:6])
		payload := make([]byte, int(msgLen)-10)
		if _, err := io.ReadFull(conn, payload); err != nil {
			continue
		}

		if msgType == 61 { // RO_ACCESS_REPORT
			items := parseReportPayload(payload)
			now := time.Now()

			for _, item := range items {
				if item.EPCHex == "" {
					continue
				}

				mu.Lock()
				tr, exists := trackers[item.EPCHex]
				if !exists {
					gtin := item.EPCHex
					if res, err := DecodeEPC(item.EPCHex); err == nil && res.GTIN14 != "" {
						gtin = res.GTIN14
					}
					isTarget := (gtin == TargetGTIN || item.EPCHex == TargetGTIN)
					tr = &TagMotionTracker{
						EPCHex:       item.EPCHex,
						GTIN:         gtin,
						IsTarget:     isTarget,
						RawRSSI:      item.PeakRSSI,
						SmoothRSSI:   item.PeakRSSI,
						History:      []RSSISample{{Timestamp: now, RSSI: item.PeakRSSI}},
						LastReadTime: now,
						TotalReads:   1,
						CurrentState: "⚪ ESTÁTICO",
					}
					trackers[item.EPCHex] = tr
					if !isTarget {
						strayReadsCount++
					}
				} else {
					tr.TotalReads++
					tr.RawRSSI = item.PeakRSSI
					tr.LastReadTime = now

					if !tr.IsTarget {
						strayReadsCount++
						mu.Unlock()
						continue
					}

					// Suavizado EMA
					tr.SmoothRSSI = (EmaAlpha * item.PeakRSSI) + ((1.0 - EmaAlpha) * tr.SmoothRSSI)

					// Registrar muestra en histórico y podar más antiguas de 1.5 segundos
					tr.History = append(tr.History, RSSISample{Timestamp: now, RSSI: tr.SmoothRSSI})
					cutoff := now.Add(-1500 * time.Millisecond)
					cleanIdx := 0
					for i, s := range tr.History {
						if s.Timestamp.After(cutoff) {
							cleanIdx = i
							break
						}
					}
					if cleanIdx > 0 {
						tr.History = tr.History[cleanIdx:]
					}

					// Calcular deltas frente a muestras pasadas (~300ms y ~1.0s)
					rssi300msAgo := tr.History[0].RSSI
					rssi1sAgo := tr.History[0].RSSI
					time300ms := now.Add(-300 * time.Millisecond)
					time1s := now.Add(-1000 * time.Millisecond)

					for _, s := range tr.History {
						if s.Timestamp.Before(time300ms) {
							rssi300msAgo = s.RSSI
						}
						if s.Timestamp.Before(time1s) {
							rssi1sAgo = s.RSSI
						}
					}

					tr.Delta300ms = tr.SmoothRSSI - rssi300msAgo
					tr.Delta1s = tr.SmoothRSSI - rssi1sAgo

					// Decisión cinemática basada en gradiente multi-escala
					if tr.Delta300ms >= 1.5 || tr.Delta1s >= MotionThreshDB {
						tr.CurrentState = "🟢 ACERCÁNDOSE"
					} else if tr.Delta300ms <= -1.5 || tr.Delta1s <= -MotionThreshDB {
						tr.CurrentState = "🔴 ALEJÁNDOSE"
					} else {
						tr.CurrentState = "⚪ ESTÁTICO"
					}
				}
				mu.Unlock()
			}
		} else if msgType == 62 { // KEEPALIVE
			ack := encodeLLRP(72, binary.BigEndian.Uint32(hdr[6:10]), nil)
			conn.Write(ack)
		}
	}
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

func estimateDistance(rssi float64) (float64, float64, string) {
	var distM float64
	var marginM float64
	var zone string

	if rssi >= 73.0 {
		distM = 0.01
		marginM = 0.01
		zone = "ON_ANTENNA_SURFACE"
	} else if rssi >= 68.0 {
		distM = 0.01 + (73.0-rssi)*0.014
		marginM = 0.02
		zone = "IMMEDIATE_PROXIMITY"
	} else if rssi >= 60.0 {
		distM = 0.08 + (68.0-rssi)*0.025
		marginM = 0.05
		zone = "NEAR_FIELD_ZONE"
	} else if rssi >= 52.0 {
		distM = 0.28 + (60.0-rssi)*0.045
		marginM = 0.10
		zone = "MID_TRANSIT_ZONE"
	} else if rssi >= 45.0 {
		distM = 0.64 + (52.0-rssi)*0.080
		marginM = 0.20
		zone = "MID_TRANSIT_ZONE"
	} else {
		distM = 1.20 + (45.0-rssi)*0.120
		marginM = 0.35
		zone = "FAR_FIELD_ZONE"
	}

	return distM, marginM, zone
}

