package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"bindunet/sdk"
)

type RawTagItem struct {
	Timestamp time.Time
	AntennaID int
	PeakRSSI  float64
	Channel   int
	EPCHex    string
}

type IDX4000Plugin struct {
	mu             sync.Mutex
	readerHost     string
	readerPort     int
	readerID       string
	deviceMAC      string
	powerDbm       uint16
	rssiThreshold  float64
	filterTags     bool
	targetEPC      string
	executionMode  string // "INFINITE", "PERIODIC", "ADHOC"
	windowDuration time.Duration
	cycleInterval  time.Duration

	running        bool
	readingActive  bool
	conn           net.Conn
	stopChan       chan struct{}
	modeChangeChan chan struct{}
	adhocChan      chan struct{}

	tagThrottleMu sync.Mutex
	lastEmitTime  map[string]time.Time
	lastEmitRSSI  map[string]float64
}

func getMacAddress() string {
	if data, err := os.ReadFile("/sys/class/net/eth0/address"); err == nil {
		mac := strings.TrimSpace(string(data))
		if mac != "" {
			return strings.ToLower(mac)
		}
	}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if (iface.Name == "eth0" || strings.HasPrefix(iface.Name, "eth")) && iface.HardwareAddr != nil {
				return strings.ToLower(iface.HardwareAddr.String())
			}
		}
	}
	return "00:50:f9:d1:0a:2b"
}

func NewIDX4000Plugin() *IDX4000Plugin {
	host := os.Getenv("LLRP_READER_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := 5084
	if pStr := os.Getenv("LLRP_READER_PORT"); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			port = p
		}
	}

	mac := getMacAddress()
	readerID := fmt.Sprintf("idx4000-%s", strings.ReplaceAll(mac, ":", "-"))

	return &IDX4000Plugin{
		readerHost:     host,
		readerPort:     port,
		readerID:       readerID,
		deviceMAC:      mac,
		powerDbm:       18,
		rssiThreshold:  -128.0,
		filterTags:     false,
		targetEPC:      "30b5d132b8112b42f9b59b2a",
		executionMode:  "INFINITE",
		windowDuration: 10 * time.Second,
		cycleInterval:  5 * time.Second,
		stopChan:       make(chan struct{}),
		modeChangeChan: make(chan struct{}, 1),
		adhocChan:      make(chan struct{}, 1),
		lastEmitTime:   make(map[string]time.Time),
		lastEmitRSSI:   make(map[string]float64),
	}
}

func (p *IDX4000Plugin) Handshake() sdk.HandshakeInfo {
	return sdk.HandshakeInfo{
		Inputs: []string{
			"RfidReaderConfig",
		},
		Outputs: []string{
			"RawTagRead",
		},
		Capabilities: []string{
			"publish",
			"subscribe",
		},
		Language:        "go",
		SDKVersion:      "bpr-sdk-go/0.1.0",
		ContractVersion: "bpr/v1",
	}
}

func (p *IDX4000Plugin) OnStart(ctx *sdk.Context) error {
	p.running = true
	ctx.Logger().Printf("[IDX4000] Plugin initialized. MAC=%s ReaderID=%s LLRP=%s:%d Mode=%s Power=%d dBm",
		p.deviceMAC, p.readerID, p.readerHost, p.readerPort, p.executionMode, p.powerDbm)

	go p.supervisorLoop(ctx)
	return nil
}

func (p *IDX4000Plugin) OnTrigger(ctx *sdk.Context, trigger sdk.Trigger) ([]sdk.Message, error) {
	ctx.Logger().Printf("[IDX4000] Trigger: %s (%s)", trigger.ID, trigger.Reason)
	return nil, nil
}

func (p *IDX4000Plugin) OnEvent(ctx *sdk.Context, event sdk.Event) ([]sdk.Message, error) {
	switch event.MessageType {
	case "RfidReaderConfig":
		return p.handleReaderConfig(ctx, event)
	default:
		ctx.Logger().Printf("[IDX4000] Ignored event type: %s", event.MessageType)
		return nil, nil
	}
}

func (p *IDX4000Plugin) handleReaderConfig(ctx *sdk.Context, event sdk.Event) ([]sdk.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	f := event.Payload
	ctx.Logger().Printf("[IDX4000] Received RfidReaderConfig: %+v", f)

	// Power
	if pwr, ok := f["tx_power_cdbm"].(float64); ok && pwr > 0 {
		if pwr > 100 {
			p.powerDbm = uint16(pwr / 100)
		} else {
			p.powerDbm = uint16(pwr)
		}
	} else if pwr, ok := f["tx_power_dbm"].(float64); ok && pwr > 0 {
		p.powerDbm = uint16(pwr)
	}

	// RSSI Threshold
	if rssi, ok := f["rssi_threshold_dbm"].(float64); ok {
		p.rssiThreshold = rssi
	}

	// Tag Filtering toggle
	if flt, ok := f["filter_tags"].(bool); ok {
		p.filterTags = flt
	}

	// Target EPC
	if target, ok := f["target_epc"].(string); ok && target != "" {
		p.targetEPC = strings.ToLower(strings.TrimSpace(target))
	}

	// Execution Mode
	if mode, ok := f["execution_mode"].(string); ok && mode != "" {
		modeUpper := strings.ToUpper(strings.TrimSpace(mode))
		if modeUpper == "CONTINUOUS" || modeUpper == "INFINITE" {
			p.executionMode = "INFINITE"
		} else if modeUpper == "PERIODIC" || modeUpper == "CYCLE" {
			p.executionMode = "PERIODIC"
		} else if modeUpper == "ADHOC" || modeUpper == "ON_DEMAND" {
			p.executionMode = "ADHOC"
		}
	}

	// Window duration
	if win, ok := f["window_duration_sec"].(float64); ok && win > 0 {
		p.windowDuration = time.Duration(win) * time.Second
	} else if win, ok := f["window_duration_sec"].(int); ok && win > 0 {
		p.windowDuration = time.Duration(win) * time.Second
	}

	// Cycle interval
	if cyc, ok := f["cycle_interval_sec"].(float64); ok && cyc > 0 {
		p.cycleInterval = time.Duration(cyc) * time.Second
	} else if cyc, ok := f["cycle_interval_sec"].(int); ok && cyc > 0 {
		p.cycleInterval = time.Duration(cyc) * time.Second
	}

	// Command
	cmd := ""
	if c, ok := f["command"].(string); ok {
		cmd = strings.ToUpper(strings.TrimSpace(c))
	}

	ctx.Logger().Printf("[IDX4000] Applied Config: Mode=%s Power=%d dBm RSSI_Cutoff=%.1f Filter=%v Target=%s Win=%v Cycle=%v Cmd=%s",
		p.executionMode, p.powerDbm, p.rssiThreshold, p.filterTags, p.targetEPC, p.windowDuration, p.cycleInterval, cmd)

	if cmd == "TRIGGER_ADHOC" || cmd == "TRIGGER" {
		select {
		case p.adhocChan <- struct{}{}:
		default:
		}
	} else if cmd == "TEST_TAG" {
		testPayload := map[string]interface{}{
			"epc":           "30b5d132b8112b42f9b59b2a",
			"device_mac":    p.deviceMAC,
			"reader_id":     p.readerID,
			"antenna":       1,
			"rssi":          56.5,
			"read_count":    1,
			"reader_vendor": "Sensormatic",
			"dock_portal":   "rfid/idx4000",
			"timestamp":     time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := ctx.Emit("RawTagRead", testPayload); err != nil {
			ctx.Logger().Printf("[IDX4000] Emit error for TEST_TAG RawTagRead: %v", err)
		} else {
			ctx.Logger().Printf("[IDX4000] 🏷 TEST_TAG Emitted RawTagRead into Bindu Mesh!")
		}
	} else if cmd == "STOP" {
		p.executionMode = "ADHOC" // Standby
		p.notifyModeChange()
	} else if cmd == "START" || cmd == "UPDATE_CONFIG" || cmd == "RESTART" {
		p.notifyModeChange()
	} else {
		p.notifyModeChange()
	}

	return nil, nil
}

func (p *IDX4000Plugin) notifyModeChange() {
	select {
	case p.modeChangeChan <- struct{}{}:
	default:
	}
}

func (p *IDX4000Plugin) supervisorLoop(ctx *sdk.Context) {
	for p.running {
		p.mu.Lock()
		mode := p.executionMode
		p.mu.Unlock()

		switch mode {
		case "INFINITE":
			p.runInfiniteMode(ctx)
		case "PERIODIC":
			p.runPeriodicMode(ctx)
		case "ADHOC":
			p.runAdhocMode(ctx)
		default:
			time.Sleep(1 * time.Second)
		}
	}
}

func (p *IDX4000Plugin) runInfiniteMode(ctx *sdk.Context) {
	ctx.Logger().Printf("[IDX4000] [INFINITE] Starting continuous reading session...")
	conn, err := p.connectAndStartROSpec(ctx)
	if err != nil {
		ctx.Logger().Printf("[IDX4000] [INFINITE] Connection error: %v. Retrying in 2s...", err)
		select {
		case <-p.stopChan:
			return
		case <-p.modeChangeChan:
			return
		case <-time.After(2 * time.Second):
			return
		}
	}
	defer p.stopAndCloseROSpec(ctx, conn)

	readStop := make(chan struct{})
	go p.readLLRPLoop(ctx, conn, readStop)

	select {
	case <-p.stopChan:
		close(readStop)
		return
	case <-p.modeChangeChan:
		close(readStop)
		return
	case <-readStop:
		ctx.Logger().Printf("[IDX4000] [INFINITE] Reader connection ended.")
		return
	}
}

func (p *IDX4000Plugin) runPeriodicMode(ctx *sdk.Context) {
	p.mu.Lock()
	win := p.windowDuration
	interval := p.cycleInterval
	p.mu.Unlock()

	ctx.Logger().Printf("[IDX4000] [PERIODIC] Active read window (%v)...", win)
	conn, err := p.connectAndStartROSpec(ctx)
	if err == nil {
		readStop := make(chan struct{})
		go p.readLLRPLoop(ctx, conn, readStop)

		select {
		case <-p.stopChan:
			close(readStop)
			p.stopAndCloseROSpec(ctx, conn)
			return
		case <-p.modeChangeChan:
			close(readStop)
			p.stopAndCloseROSpec(ctx, conn)
			return
		case <-time.After(win):
			close(readStop)
			p.stopAndCloseROSpec(ctx, conn)
		case <-readStop:
			p.stopAndCloseROSpec(ctx, conn)
		}
	} else {
		ctx.Logger().Printf("[IDX4000] [PERIODIC] LLRP connect error: %v", err)
	}

	ctx.Logger().Printf("[IDX4000] [PERIODIC] Sleeping for cycle interval (%v)...", interval)
	select {
	case <-p.stopChan:
		return
	case <-p.modeChangeChan:
		return
	case <-time.After(interval):
		return
	}
}

func (p *IDX4000Plugin) runAdhocMode(ctx *sdk.Context) {
	ctx.Logger().Printf("[IDX4000] [ADHOC] Reader in standby. Awaiting trigger...")
	select {
	case <-p.stopChan:
		return
	case <-p.modeChangeChan:
		return
	case <-p.adhocChan:
		p.mu.Lock()
		win := p.windowDuration
		p.mu.Unlock()

		ctx.Logger().Printf("[IDX4000] [ADHOC] ⚡ Trigger activated! Reading for %v window...", win)
		conn, err := p.connectAndStartROSpec(ctx)
		if err != nil {
			ctx.Logger().Printf("[IDX4000] [ADHOC] LLRP start failed: %v", err)
			return
		}

		readStop := make(chan struct{})
		go p.readLLRPLoop(ctx, conn, readStop)

		select {
		case <-p.stopChan:
			close(readStop)
			p.stopAndCloseROSpec(ctx, conn)
			return
		case <-p.modeChangeChan:
			close(readStop)
			p.stopAndCloseROSpec(ctx, conn)
			return
		case <-time.After(win):
			ctx.Logger().Printf("[IDX4000] [ADHOC] Window expired (%v). Stopping ROSpec.", win)
			close(readStop)
			p.stopAndCloseROSpec(ctx, conn)
		case <-readStop:
			p.stopAndCloseROSpec(ctx, conn)
		}
	}
}

func (p *IDX4000Plugin) connectAndStartROSpec(ctx *sdk.Context) (net.Conn, error) {
	p.mu.Lock()
	addr := fmt.Sprintf("%s:%d", p.readerHost, p.readerPort)
	power := p.powerDbm
	p.mu.Unlock()

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}

	// 1. Read initial LLRP notification
	hdr := make([]byte, 10)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		conn.Close()
		return nil, fmt.Errorf("initial notification read failed: %w", err)
	}
	msgLen := binary.BigEndian.Uint32(hdr[2:6])
	if msgLen > 10 {
		body := make([]byte, int(msgLen)-10)
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		_, _ = io.ReadFull(conn, body)
		_ = conn.SetReadDeadline(time.Time{})
	}

	// 2. DELETE_ROSPEC 1
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(encodeLLRP(21, 101, uint32Bytes(1)))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.ReadFull(conn, hdr)
	l := binary.BigEndian.Uint32(hdr[2:6])
	if l > 10 {
		b := make([]byte, int(l)-10)
		_, _ = io.ReadFull(conn, b)
	}

	// 3. ADD_ROSPEC 1 with configured power
	roSpecPayload := buildStrictROSpec(1, 0, power)
	_, err = conn.Write(encodeLLRP(20, 102, roSpecPayload))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ADD_ROSPEC write failed: %w", err)
	}
	_, _ = io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	if l > 10 {
		b := make([]byte, int(l)-10)
		_, _ = io.ReadFull(conn, b)
		desc, code := parseStatus(b)
		ctx.Logger().Printf("[IDX4000] ADD_ROSPEC Result: Code=%d Status=%s", code, desc)
		if code != 0 {
			// Fallback to basic spec without RFTransmitter if rejected
			ctx.Logger().Printf("[IDX4000] Retrying ADD_ROSPEC with default RF spec...")
			basicPayload := buildBasicROSpec(1)
			_, _ = conn.Write(encodeLLRP(20, 105, basicPayload))
			_, _ = io.ReadFull(conn, hdr)
			l2 := binary.BigEndian.Uint32(hdr[2:6])
			if l2 > 10 {
				b2 := make([]byte, int(l2)-10)
				_, _ = io.ReadFull(conn, b2)
				d2, c2 := parseStatus(b2)
				ctx.Logger().Printf("[IDX4000] Basic ADD_ROSPEC Result: Code=%d Status=%s", c2, d2)
			}
		}
	}

	// 4. ENABLE_ROSPEC 1
	_, err = conn.Write(encodeLLRP(24, 103, uint32Bytes(1)))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ENABLE_ROSPEC write failed: %w", err)
	}
	_, _ = io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	if l > 10 {
		b := make([]byte, int(l)-10)
		_, _ = io.ReadFull(conn, b)
		desc, code := parseStatus(b)
		ctx.Logger().Printf("[IDX4000] ENABLE_ROSPEC Result: Code=%d Status=%s", code, desc)
	}

	// 5. START_ROSPEC 1
	_, err = conn.Write(encodeLLRP(22, 104, uint32Bytes(1)))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("START_ROSPEC write failed: %w", err)
	}
	_, _ = io.ReadFull(conn, hdr)
	l = binary.BigEndian.Uint32(hdr[2:6])
	if l > 10 {
		b := make([]byte, int(l)-10)
		_, _ = io.ReadFull(conn, b)
		desc, code := parseStatus(b)
		ctx.Logger().Printf("[IDX4000] START_ROSPEC Result: Code=%d Status=%s", code, desc)
	}

	_ = conn.SetWriteDeadline(time.Time{})
	_ = conn.SetReadDeadline(time.Time{})

	p.mu.Lock()
	p.readingActive = true
	p.conn = conn
	p.mu.Unlock()

	ctx.Logger().Printf("[IDX4000] ✅ ROSpec #1 active and radiating at %d dBm.", power)
	return conn, nil
}

func (p *IDX4000Plugin) stopAndCloseROSpec(ctx *sdk.Context, conn net.Conn) {
	if conn == nil {
		return
	}
	p.mu.Lock()
	p.readingActive = false
	if p.conn == conn {
		p.conn = nil
	}
	p.mu.Unlock()

	_ = conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
	// STOP_ROSPEC 1
	_, _ = conn.Write(encodeLLRP(23, 999, uint32Bytes(1)))
	time.Sleep(50 * time.Millisecond)
	conn.Close()
	ctx.Logger().Printf("[IDX4000] ⏹ ROSpec stopped & LLRP connection closed.")
}

func (p *IDX4000Plugin) readLLRPLoop(ctx *sdk.Context, conn net.Conn, readStop chan struct{}) {
	hdr := make([]byte, 10)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(conn, hdr); err != nil {
			select {
			case <-readStop:
				return
			default:
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				close(readStop)
				return
			}
		}

		msgType := binary.BigEndian.Uint16(hdr[0:2]) & 0x03FF
		msgLen := binary.BigEndian.Uint32(hdr[2:6])
		if msgLen < 10 {
			continue
		}

		body := make([]byte, int(msgLen)-10)
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(conn, body); err != nil {
			close(readStop)
			return
		}

		if msgType == 62 { // KEEPALIVE
			ack := encodeLLRP(72, binary.BigEndian.Uint32(hdr[6:10]), nil)
			_, _ = conn.Write(ack)
			continue
		}

		if msgType == 61 { // RO_ACCESS_REPORT
			items := parseROAccessReport(body)
			for _, item := range items {
				p.handleRawTag(ctx, item)
			}
		}
	}
}

func (p *IDX4000Plugin) handleRawTag(ctx *sdk.Context, item RawTagItem) {
	if item.EPCHex == "" {
		return
	}

	p.mu.Lock()
	rssiThreshold := p.rssiThreshold
	filterTags := p.filterTags
	targetEPC := p.targetEPC
	mac := p.deviceMAC
	readerID := p.readerID
	p.mu.Unlock()

	// RSSI Filter
	if item.PeakRSSI < rssiThreshold {
		return
	}

	// Target EPC Filter
	epcLower := strings.ToLower(item.EPCHex)
	if filterTags && targetEPC != "" {
		if !strings.EqualFold(epcLower, targetEPC) && !strings.Contains(epcLower, targetEPC) {
			return
		}
	}

	// Rate limiting / deduplication: Max 10 emits/sec per tag unless RSSI jumped > 2 dBm
	now := time.Now()
	p.tagThrottleMu.Lock()
	lastTime, hasLast := p.lastEmitTime[epcLower]
	lastRSSI := p.lastEmitRSSI[epcLower]
	rssiDelta := item.PeakRSSI - lastRSSI
	if rssiDelta < 0 {
		rssiDelta = -rssiDelta
	}

	if hasLast && now.Sub(lastTime) < 100*time.Millisecond && rssiDelta < 2.0 {
		p.tagThrottleMu.Unlock()
		return
	}
	p.lastEmitTime[epcLower] = now
	p.lastEmitRSSI[epcLower] = item.PeakRSSI
	p.tagThrottleMu.Unlock()

	payload := map[string]interface{}{
		"epc":           epcLower,
		"device_mac":    mac,
		"reader_id":     readerID,
		"antenna":       item.AntennaID,
		"rssi":          item.PeakRSSI,
		"read_count":    1,
		"reader_vendor": "Sensormatic",
		"dock_portal":   "rfid/idx4000",
		"timestamp":     item.Timestamp.Format(time.RFC3339Nano),
	}

	if err := ctx.Emit("RawTagRead", payload); err != nil {
		ctx.Logger().Printf("[IDX4000] Emit error for RawTagRead: %v", err)
	} else {
		ctx.Logger().Printf("[IDX4000] 🏷 Tag Emitted: EPC=%s RSSI=%.1f dBm Ant=%d",
			epcLower, item.PeakRSSI, item.AntennaID)
	}
}

func parseROAccessReport(data []byte) []RawTagItem {
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
			offset++
		} else {
			if offset+4 > len(data) {
				break
			}
			pType := binary.BigEndian.Uint16(data[offset:offset+2]) & 0x03FF
			pLen := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
			if pLen < 4 || offset+pLen > len(data) {
				break
			}
			if pType == 241 && pLen >= 6 { // EPCData
				epcLen := int(binary.BigEndian.Uint16(data[offset+4:offset+6])) / 8
				if offset+6+epcLen <= offset+pLen {
					item.EPCHex = hex.EncodeToString(data[offset+6 : offset+6+epcLen])
				}
			}
			offset += pLen
		}
	}
	return item
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
	var antList []byte
	if antennaID == 0 {
		antList = []byte{0, 1, 0, 0} // All antennas
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

func buildBasicROSpec(roSpecID uint32) []byte {
	var boundary bytes.Buffer
	boundary.Write(buildTLV(179, []byte{1}))
	stopTrig := make([]byte, 5)
	boundary.Write(buildTLV(182, stopTrig))
	boundaryTLV := buildTLV(178, boundary.Bytes())

	var aiSpec bytes.Buffer
	antList := []byte{0, 1, 0, 0}
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

func parseStatus(data []byte) (string, uint16) {
	for offset := 0; offset+4 <= len(data); {
		pType := binary.BigEndian.Uint16(data[offset:offset+2]) & 0x03FF
		pLen := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if pType == 287 && pLen >= 8 {
			code := binary.BigEndian.Uint16(data[offset+4 : offset+6])
			descLen := int(binary.BigEndian.Uint16(data[offset+6 : offset+8]))
			desc := "Success"
			if descLen > 0 && offset+8+descLen <= len(data) {
				desc = string(data[offset+8 : offset+8+descLen])
			}
			return desc, code
		}
		offset += pLen
	}
	return "Unknown", 999
}

func (p *IDX4000Plugin) Shutdown(ctx *sdk.Context) error {
	p.mu.Lock()
	p.running = false
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
	p.mu.Unlock()
	close(p.stopChan)
	ctx.Logger().Printf("[IDX4000] Plugin shutdown complete.")
	return nil
}

func main() {
	plugin := NewIDX4000Plugin()
	if err := sdk.Run(plugin); err != nil {
		log.Fatalf("IDX4000 plugin failed: %v", err)
	}
}
