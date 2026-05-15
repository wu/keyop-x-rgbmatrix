package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mcuadros/go-rpi-rgb-led-matrix"
	"github.com/wu/keyop-messenger"
	"github.com/wu/keyop/core"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

//go:embed resources
var resources embed.FS

// Payload type strings matching the canonical values defined in the respective service packages.
const (
	moonEventType           = "service.moon.v1"
	sunEventType            = "service.sun.v1"
	weatherEventType        = "service.weather.v1"
	weatherRequestEventType = "service.weather.request.v1"
	tideEventType           = "service.tide.v1"
	auroraEventType         = "service.aurora.v1"
)

// infoSegment is one coloured run of text within an infoMessage.
type infoSegment struct {
	Text  string
	Color *color.RGBA // nil → use the message's Color (or default cyan)
}

// infoMessage holds a rotating bottom-row display item with staleness tracking.
type infoMessage struct {
	Label     string
	TargetAt  time.Time
	UpdatedAt time.Time
	FormatFn  func(label string, target time.Time) string // nil → formatCountdown
	Icon      image.Image                                 // optional left-side icon (nil if none)
	Color     *color.RGBA                                 // optional text color (nil → default cyan)
	Segments  []infoSegment                               // when set, rendered instead of Label
}

// moonPayload extracts the fields we need from service.moon.v1 events.
type moonPayload struct {
	NextMajorName string `json:"next_major_name"`
	NextMajorTime string `json:"next_major_time"`
}

// sunPayload extracts the fields we need from service.sun.v1 events.
type sunPayload struct {
	CivilDawn    time.Time `json:"civil_dawn"`
	CivilDusk    time.Time `json:"civil_dusk"`
	TomorrowDawn time.Time `json:"tomorrow_dawn"`
}

// weatherPeriod extracts the fields we need from a ForecastPeriod.
type weatherPeriod struct {
	IsDaytime   bool    `json:"isDaytime"`
	Temperature float64 `json:"temperature"`
}

// weatherPayload extracts the fields we need from service.weather.v1 events.
type weatherPayload struct {
	Periods []weatherPeriod `json:"periods"`
}

// weatherRequest is the payload for a service.weather.request.v1 message.
type weatherRequest struct {
	RequestID string `json:"requestId"`
}

// tidePayload extracts the fields we need from service.tide.v1 events.
// TideRecord.Value uses json:"v,string" because the tides service encodes it as a JSON string.
type tidePayload struct {
	Current struct {
		Time  string  `json:"t"`
		Value float64 `json:"v,string"`
	} `json:"current"`
	State    string `json:"state"`
	NextPeak *struct {
		Time  string  `json:"time"`
		Value float64 `json:"value"`
		Type  string  `json:"type"`
	} `json:"nextPeak"`
}

type RGBMatrixPlugin struct {
	deps       core.Dependencies
	cfg        core.ServiceConfig
	matrix     rgbmatrix.Matrix
	canvas     *rgbmatrix.Canvas
	bigFace    font.Face
	smallFace  font.Face
	mediumFace font.Face
	swapGB     bool
	ctx        context.Context

	mu           sync.RWMutex
	temps        map[string]float64
	statuses     map[string]string
	lastUpdate   map[string]time.Time
	tempNames    []string
	tempIdx      int
	nameMap      map[string]string
	mainTempName string

	infoMessages              map[string]infoMessage
	infoKeys                  []string
	infoIdx                   int
	moonChannelName           string
	sunChannelName            string
	weatherChannelName        string
	weatherRequestChannelName string
	tidesChannelName          string
	auroraChannelName         string
}

// Initialize performs one-time startup required by the service (resource loading or connectivity checks).
func (p *RGBMatrixPlugin) Initialize() error {
	p.deps.MustGetLogger().Info("RGBMatrixPlugin initializing")

	config := &rgbmatrix.DefaultConfig
	config.Rows = 32
	config.Cols = 64
	config.Parallel = 1
	config.ChainLength = 1
	config.Brightness = 50
	config.HardwareMapping = "adafruit-hat"
	config.ShowRefreshRate = false
	config.InverseColors = false
	config.DisableHardwarePulsing = false

	// Allow overrides from config
	if v, ok := p.cfg.Config["rows"].(float64); ok {
		config.Rows = int(v)
	}
	if v, ok := p.cfg.Config["cols"].(float64); ok {
		config.Cols = int(v)
	}
	if v, ok := p.cfg.Config["swap_gb"].(bool); ok {
		p.swapGB = v
	}
	if v, ok := p.cfg.Config["main_temp_name"].(string); ok {
		p.mainTempName = v
	}

	m, err := rgbmatrix.NewRGBLedMatrix(config)
	if err != nil {
		return fmt.Errorf("failed to initialize matrix: %w", err)
	}

	p.matrix = m
	p.canvas = rgbmatrix.NewCanvas(p.matrix)
	p.temps = make(map[string]float64)
	p.statuses = make(map[string]string)
	p.lastUpdate = make(map[string]time.Time)
	p.nameMap = make(map[string]string)
	p.infoMessages = make(map[string]infoMessage)

	if v, ok := p.cfg.Config["temp_name_map"].(map[string]interface{}); ok {
		for k, val := range v {
			if s, ok := val.(string); ok {
				p.nameMap[k] = s
			}
		}
	}

	p.bigFace, err = loadFontFace("resources/pixelmix-8.ttf", 8)
	if err != nil {
		return err
	}

	p.mediumFace, err = loadFontFace("resources/pixelation-7.ttf", 7)
	if err != nil {
		return err
	}

	p.smallFace, err = loadFontFace("resources/pixel-letters-6.ttf", 6)
	if err != nil {
		return err
	}

	err = p.canvas.Clear()
	if err != nil {
		return fmt.Errorf("failed to clear canvas: %w", err)
	}

	p.deps.MustGetLogger().Info("RGBMatrixPlugin initialized", "rows", config.Rows, "cols", config.Cols)
	return nil
}

func loadFontFace(path string, size float64) (font.Face, error) {
	fontBytes, err := resources.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read font file %s: %w", path, err)
	}
	f, err := opentype.Parse(fontBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse font %s: %w", path, err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create font face from %s: %w", path, err)
	}
	return face, nil
}

func (p *RGBMatrixPlugin) colorRGBA(r, g, b, a uint8) color.RGBA {
	if p.swapGB {
		return color.RGBA{r, b, g, a}
	}
	return color.RGBA{r, g, b, a}
}

// swapGBImage wraps an image.Image and swaps the green and blue channels in At().
type swapGBImage struct{ image.Image }

func (s swapGBImage) At(x, y int) color.Color {
	r, g, b, a := s.Image.At(x, y).RGBA()
	return color.RGBA64{R: uint16(r), G: uint16(b), B: uint16(g), A: uint16(a)}
}

// Check performs the service's periodic work: collect data, evaluate state, and publish messages/metrics.
func (p *RGBMatrixPlugin) Check() error {
	logger := p.deps.MustGetLogger()
	logger.Info("RGBMatrixPlugin Check called")

	newMsgr := p.deps.MustGetMessenger()
	if newMsgr == nil {
		return fmt.Errorf("rgbmatrix: new messenger not initialized")
	}

	// Register payload types
	if err := newMsgr.RegisterPayloadType("core.status.v1", &core.StatusEvent{}); err != nil {
		if !core.IsDuplicatePayloadRegistration(err) {
			logger.Error("rgbmatrix: failed to register core.status.v1", "error", err)
			return err
		}
	}

	// Subscribe to status channel
	subscriberID := "rgbmatrix-status"
	logger.Info("rgbmatrix: subscribing to status channel", "subscriberID", subscriberID)
	if err := newMsgr.Subscribe(p.ctx, "status", subscriberID, func(msgCtx context.Context, msg messenger.Message) error {
		p.mu.Lock()
		defer p.mu.Unlock()

		// Decode the status event
		var statusEvent *core.StatusEvent
		if se, ok := msg.Payload.(*core.StatusEvent); ok {
			statusEvent = se
		} else if se, ok := msg.Payload.(core.StatusEvent); ok {
			statusEvent = &se
		} else {
			return nil
		}

		metric, ok := core.ExtractSourcePayload[core.MetricEvent](statusEvent)
		if !ok || metric.SourcePayloadType != "core.temp.v1" {
			return nil
		}
		tempEvent, ok := core.ExtractMetricSourcePayload[core.TempEvent](metric)
		if !ok {
			return nil
		}

		sensorName := tempEvent.SensorName
		p.temps[sensorName] = metric.Value
		p.statuses[sensorName] = statusEvent.Status
		p.lastUpdate[sensorName] = msg.Timestamp

		// Update sorted names
		names := make([]string, 0, len(p.temps))
		for name := range p.temps {
			names = append(names, name)
		}
		sort.Strings(names)
		p.tempNames = names

		logger.Info("Got temp status update", "sensorName", sensorName, "tempF", metric.Value, "status", statusEvent.Status, "totalTemps", len(p.temps))
		return nil
	}); err != nil {
		logger.Error("rgbmatrix: failed to subscribe to status channel", "error", err)
		return fmt.Errorf("rgbmatrix: failed to subscribe to status channel: %w", err)
	}

	// Subscribe to moon channel (optional)
	if p.moonChannelName != "" {
		logger.Info("rgbmatrix: subscribing to moon channel", "channel", p.moonChannelName)
		if err := newMsgr.Subscribe(p.ctx, p.moonChannelName, "rgbmatrix-moon", func(msgCtx context.Context, msg messenger.Message) error {
			if msg.PayloadType != moonEventType {
				return nil
			}
			mp, ok := extractMoonPayload(msg)
			if !ok {
				return nil
			}
			targetTime, err := time.Parse(time.RFC3339, mp.NextMajorTime)
			if err != nil {
				logger.Warn("rgbmatrix: failed to parse moon target time", "error", err, "value", mp.NextMajorTime)
				return nil
			}
			icon := loadIcon(moonPhaseIconPath(mp.NextMajorName))
			p.mu.Lock()
			defer p.mu.Unlock()
			p.infoMessages["moon"] = infoMessage{
				Label:     shortMoonLabel(mp.NextMajorName),
				TargetAt:  targetTime,
				UpdatedAt: msg.Timestamp,
				FormatFn:  formatCountdown,
				Icon:      icon,
			}
			p.rebuildInfoKeysLocked()
			logger.Info("rgbmatrix: received moon event", "next", mp.NextMajorName, "at", targetTime)
			return nil
		}); err != nil {
			logger.Error("rgbmatrix: failed to subscribe to moon channel", "channel", p.moonChannelName, "error", err)
			return fmt.Errorf("rgbmatrix: failed to subscribe to moon channel: %w", err)
		}
	}

	// Subscribe to sun channel (optional)
	if p.sunChannelName != "" {
		logger.Info("rgbmatrix: subscribing to sun channel", "channel", p.sunChannelName)
		if err := newMsgr.Subscribe(p.ctx, p.sunChannelName, "rgbmatrix-sun", func(msgCtx context.Context, msg messenger.Message) error {
			if msg.PayloadType != sunEventType {
				return nil
			}
			sp, ok := extractSunPayload(msg)
			if !ok {
				return nil
			}
			now := time.Now()
			var iconPath string
			var targetAt time.Time
			dawnFuture := sp.CivilDawn.After(now)
			duskFuture := sp.CivilDusk.After(now)
			switch {
			case dawnFuture && duskFuture:
				if sp.CivilDawn.Before(sp.CivilDusk) {
					iconPath, targetAt = "resources/dawn.png", sp.CivilDawn
				} else {
					iconPath, targetAt = "resources/dusk.png", sp.CivilDusk
				}
			case dawnFuture:
				iconPath, targetAt = "resources/dawn.png", sp.CivilDawn
			case duskFuture:
				iconPath, targetAt = "resources/dusk.png", sp.CivilDusk
			default:
				// All of today's events have passed — use tomorrow's dawn
				if !sp.TomorrowDawn.IsZero() && sp.TomorrowDawn.After(now) {
					iconPath, targetAt = "resources/dawn.png", sp.TomorrowDawn
				} else {
					return nil
				}
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			p.infoMessages["sun"] = infoMessage{
				Label:     "",
				TargetAt:  targetAt,
				UpdatedAt: msg.Timestamp,
				Icon:      loadIcon(iconPath),
			}
			p.rebuildInfoKeysLocked()
			logger.Info("rgbmatrix: received sun event", "next", iconPath, "at", targetAt)
			return nil
		}); err != nil {
			logger.Error("rgbmatrix: failed to subscribe to sun channel", "channel", p.sunChannelName, "error", err)
			return fmt.Errorf("rgbmatrix: failed to subscribe to sun channel: %w", err)
		}
	}

	// Subscribe to weather channel (optional)
	if p.weatherChannelName != "" {
		logger.Info("rgbmatrix: subscribing to weather channel", "channel", p.weatherChannelName)
		if err := newMsgr.Subscribe(p.ctx, p.weatherChannelName, "rgbmatrix-weather", func(msgCtx context.Context, msg messenger.Message) error {
			if msg.PayloadType != weatherEventType {
				return nil
			}
			label, ok := extractWeatherLabel(msg)
			if !ok {
				return nil
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			p.infoMessages["weather"] = infoMessage{
				Label:     label,
				UpdatedAt: msg.Timestamp,
			}
			p.rebuildInfoKeysLocked()
			logger.Info("rgbmatrix: received weather event", "label", label)
			return nil
		}); err != nil {
			logger.Error("rgbmatrix: failed to subscribe to weather channel", "channel", p.weatherChannelName, "error", err)
			return fmt.Errorf("rgbmatrix: failed to subscribe to weather channel: %w", err)
		}
	}

	// Subscribe to tides channel (optional)
	if p.tidesChannelName != "" {
		logger.Info("rgbmatrix: subscribing to tides channel", "channel", p.tidesChannelName)
		if err := newMsgr.Subscribe(p.ctx, p.tidesChannelName, "rgbmatrix-tides", func(msgCtx context.Context, msg messenger.Message) error {
			if msg.PayloadType != tideEventType {
				return nil
			}
			b, err := json.Marshal(msg.Payload)
			if err != nil {
				return nil
			}
			var tp tidePayload
			if err := json.Unmarshal(b, &tp); err != nil {
				return nil
			}

			p.mu.Lock()
			defer p.mu.Unlock()

			red := &color.RGBA{R: 255, G: 0, B: 0, A: 255}
			tideColor := func(v float64) *color.RGBA {
				if v > 10 || v < 0 {
					return red
				}
				return nil
			}
			var segments []infoSegment
			segments = append(segments, infoSegment{
				Text:  tideFeetInches(tp.Current.Value),
				Color: tideColor(tp.Current.Value),
			})
			if tp.NextPeak != nil && tp.NextPeak.Type != "" {
				segments = append(segments, infoSegment{Text: " "})
				segments = append(segments, infoSegment{
					Text:  tideFeetInches(tp.NextPeak.Value),
					Color: tideColor(tp.NextPeak.Value),
				})
			}
			tideIconPath := ""
			switch tp.State {
			case "rising":
				tideIconPath = "resources/tide_rising.png"
			case "falling":
				tideIconPath = "resources/tide_falling.png"
			}
			p.infoMessages["tides"] = infoMessage{
				Segments:  segments,
				UpdatedAt: msg.Timestamp,
				Icon:      loadIcon(tideIconPath),
			}

			p.rebuildInfoKeysLocked()
			return nil
		}); err != nil {
			logger.Error("rgbmatrix: failed to subscribe to tides channel", "channel", p.tidesChannelName, "error", err)
			return fmt.Errorf("rgbmatrix: failed to subscribe to tides channel: %w", err)
		}
	}

	// Subscribe to aurora channel (optional)
	if p.auroraChannelName != "" {
		logger.Info("rgbmatrix: subscribing to aurora channel", "channel", p.auroraChannelName)
		if err := newMsgr.Subscribe(p.ctx, p.auroraChannelName, "rgbmatrix-aurora", func(msgCtx context.Context, msg messenger.Message) error {
			if msg.PayloadType != auroraEventType {
				return nil
			}
			b, err := json.Marshal(msg.Payload)
			if err != nil {
				return nil
			}
			var ap struct {
				Likelihood int `json:"likelihood"`
			}
			if err := json.Unmarshal(b, &ap); err != nil {
				return nil
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if ap.Likelihood == 0 {
				delete(p.infoMessages, "aurora")
			} else {
				pink := color.RGBA{R: 255, G: 105, B: 180, A: 255}
				p.infoMessages["aurora"] = infoMessage{
					Label:     fmt.Sprintf("aurora %d%%", ap.Likelihood),
					UpdatedAt: msg.Timestamp,
					Color:     &pink,
				}
			}
			p.rebuildInfoKeysLocked()
			logger.Info("rgbmatrix: received aurora event", "likelihood", ap.Likelihood)
			return nil
		}); err != nil {
			logger.Error("rgbmatrix: failed to subscribe to aurora channel", "channel", p.auroraChannelName, "error", err)
			return fmt.Errorf("rgbmatrix: failed to subscribe to aurora channel: %w", err)
		}
	}

	// Request an immediate weather forecast so the display populates without waiting for the next scheduled check
	if p.weatherRequestChannelName != "" {
		req := &weatherRequest{RequestID: "rgbmatrix-startup"}
		if err := newMsgr.Publish(p.ctx, p.weatherRequestChannelName, weatherRequestEventType, req); err != nil {
			logger.Warn("rgbmatrix: failed to publish weather request", "error", err)
		}
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	cycleTicker := time.NewTicker(3 * time.Second)
	defer cycleTicker.Stop()

	infoCycleTicker := time.NewTicker(5 * time.Second)
	defer infoCycleTicker.Stop()

	// Initial render
	if err := p.Render(); err != nil {
		logger.Error("Failed to render", "error", err)
	}

	for {
		select {
		case <-p.ctx.Done():
			logger.Info("RGBMatrixPlugin Check context cancelled")
			err := p.canvas.Clear()
			if err != nil {
				return fmt.Errorf("failed to clear canvas: %w", err)
			}
			return p.ctx.Err()
		case <-cycleTicker.C:
			p.mu.Lock()
			if len(p.tempNames) > 0 {
				p.tempIdx = (p.tempIdx + 1) % len(p.tempNames)
			}
			p.mu.Unlock()
		case <-infoCycleTicker.C:
			p.mu.Lock()
			if len(p.infoKeys) > 0 {
				p.infoIdx = (p.infoIdx + 1) % len(p.infoKeys)
			}
			p.mu.Unlock()
		case <-ticker.C:
			if err := p.Render(); err != nil {
				logger.Error("Failed to render", "error", err)
			}
		}
	}
}

func (p *RGBMatrixPlugin) Render() error {

	if p.canvas == nil {
		return fmt.Errorf("canvas not initialized")
	}

	t := time.Now()
	timeStr := t.Format("3:04pm")
	dayOfWeek := t.Format("Mon")
	dayOfMonth := t.Format("_2") // Day of month (1-31)

	// Prune stale info messages and rebuild key list (requires write lock)
	p.mu.Lock()
	for key, msg := range p.infoMessages {
		if time.Since(msg.UpdatedAt) > 30*time.Minute {
			delete(p.infoMessages, key)
		}
	}
	p.rebuildInfoKeysLocked()
	p.mu.Unlock()

	// Create an image to draw the text onto
	bounds := p.canvas.Bounds()
	img := image.NewRGBA(bounds)

	// Draw text to the image
	d := &font.Drawer{
		Dst: img,
		Src: image.NewUniform(p.colorRGBA(100, 0, 255, 255)),
	}

	// 1. Draw Day of Week (Upper Left)
	d.Face = p.smallFace
	d.Dot = fixed.Point26_6{X: fixed.I(0), Y: fixed.I(6)}
	d.DrawString(dayOfWeek)

	// 2. Draw Day of Month (Upper Left, Below Day of Week)
	d.Face = p.mediumFace
	d.Dot = fixed.Point26_6{X: fixed.I(0), Y: fixed.I(11)}
	d.DrawString(dayOfMonth)

	// 3. Draw Time (Top Center/Right)
	d.Face = p.bigFace
	d.Dot = fixed.Point26_6{X: fixed.I(20), Y: fixed.I(10)}
	d.DrawString(timeStr)

	// 4. Draw Temperature (Middle)
	p.mu.RLock()
	if len(p.tempNames) > 0 {
		if p.tempIdx >= len(p.tempNames) {
			p.tempIdx = 0
		}
		if p.mainTempName != "" && p.tempNames[p.tempIdx] == p.mainTempName {
			// Skip main temp in cycle since it's always shown separately
			p.tempIdx = (p.tempIdx + 1) % len(p.tempNames)
		}
		name := p.tempNames[p.tempIdx]
		temp := p.temps[name]

		mainTemp, ok := p.temps[p.mainTempName]
		if !ok {
			mainTemp = 0.0
		}

		displayName := name
		if mappedName, ok := p.nameMap[name]; ok {
			displayName = mappedName
		} else {
			// Fallback to existing logic if no mapping exists
			nameIdx := strings.Index(name, ".")
			if nameIdx != -1 {
				displayName = name[nameIdx+1:]
			}
			if len(displayName) > 1 {
				displayName = displayName[0:1]
			}
		}
		mainTempStr := fmt.Sprintf("%.1f", mainTemp)
		d.Face = p.bigFace
		d.Src = image.NewUniform(p.getTempColorWithStaleness(p.statuses[p.mainTempName], p.lastUpdate[p.mainTempName]))
		d.Dot = fixed.Point26_6{X: fixed.I(0), Y: fixed.I(20)}
		d.DrawString(mainTempStr)

		tempStr := fmt.Sprintf("%.1f", temp)
		d.Face = p.bigFace
		d.Src = image.NewUniform(p.getTempColorWithStaleness(p.statuses[name], p.lastUpdate[name]))
		d.Dot = fixed.Point26_6{X: fixed.I(26), Y: fixed.I(20)}
		d.DrawString(tempStr)

		tempNameStr := fmt.Sprintf("%s", displayName)
		d.Face = p.bigFace
		d.Src = image.NewUniform(p.getTempColorWithStaleness(p.statuses[name], p.lastUpdate[name]))
		d.Dot = fixed.Point26_6{X: fixed.I(52), Y: fixed.I(20)}
		d.DrawString(tempNameStr)
	}

	// 5. Draw rotating info message (Bottom Row)
	if len(p.infoKeys) > 0 {
		idx := p.infoIdx
		if idx >= len(p.infoKeys) {
			idx = 0
		}
		msg := p.infoMessages[p.infoKeys[idx]]
		d.Face = p.bigFace
		defaultColor := p.colorRGBA(0, 180, 180, 255)
		if msg.Color != nil {
			defaultColor = p.colorRGBA(msg.Color.R, msg.Color.G, msg.Color.B, msg.Color.A)
		}
		d.Src = image.NewUniform(defaultColor)
		textX := 0
		if msg.Icon != nil {
			ib := msg.Icon.Bounds()
			src := image.Image(msg.Icon)
			if p.swapGB {
				src = swapGBImage{msg.Icon}
			}
			draw.Draw(img, image.Rect(1, 31-ib.Dy(), 1+ib.Dx(), 31), src, ib.Min, draw.Over)
			textX = 1 + ib.Dx() + 5
		}
		d.Dot = fixed.Point26_6{X: fixed.I(textX), Y: fixed.I(31)}
		if len(msg.Segments) > 0 {
			for _, seg := range msg.Segments {
				if seg.Color != nil {
					d.Src = image.NewUniform(p.colorRGBA(seg.Color.R, seg.Color.G, seg.Color.B, seg.Color.A))
				} else {
					d.Src = image.NewUniform(defaultColor)
				}
				d.DrawString(seg.Text)
			}
		} else {
			format := formatCountdown
			if msg.FormatFn != nil {
				format = msg.FormatFn
			}
			d.DrawString(format(msg.Label, msg.TargetAt))
		}
	}

	p.mu.RUnlock()

	// Copy image pixels to canvas
	draw.Draw(p.canvas, bounds, img, image.Point{}, draw.Over)

	p.canvas.Render()

	return nil
}

// rebuildInfoKeysLocked rebuilds the sorted infoKeys slice and clamps infoIdx.
// Must be called while holding p.mu (write lock).
func (p *RGBMatrixPlugin) rebuildInfoKeysLocked() {
	keys := make([]string, 0, len(p.infoMessages))
	for k := range p.infoMessages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	p.infoKeys = keys
	if len(p.infoKeys) == 0 {
		p.infoIdx = 0
	} else if p.infoIdx >= len(p.infoKeys) {
		p.infoIdx = 0
	}
}

// extractMoonPayload extracts moon countdown fields from a messenger message via JSON re-encoding.
// This works regardless of which concrete type the messenger decoded the payload into.
func extractMoonPayload(msg messenger.Message) (*moonPayload, bool) {
	b, err := json.Marshal(msg.Payload)
	if err != nil {
		return nil, false
	}
	var mp moonPayload
	if err := json.Unmarshal(b, &mp); err != nil || mp.NextMajorName == "" || mp.NextMajorTime == "" {
		return nil, false
	}
	return &mp, true
}

// extractSunPayload extracts sun countdown fields from a messenger message via JSON re-encoding.
func extractSunPayload(msg messenger.Message) (*sunPayload, bool) {
	b, err := json.Marshal(msg.Payload)
	if err != nil {
		return nil, false
	}
	var sp sunPayload
	if err := json.Unmarshal(b, &sp); err != nil || (sp.CivilDawn.IsZero() && sp.CivilDusk.IsZero()) {
		return nil, false
	}
	return &sp, true
}

// extractWeatherLabel builds a weather display string from a forecast message.
// During the day the first period is daytime: shows "hi X lo Y".
// At night the first period is nighttime: shows "lo X hi Y" (tonight's low then tomorrow's high).
func extractWeatherLabel(msg messenger.Message) (string, bool) {
	b, err := json.Marshal(msg.Payload)
	if err != nil {
		return "", false
	}
	var wp weatherPayload
	if err := json.Unmarshal(b, &wp); err != nil || len(wp.Periods) == 0 {
		return "", false
	}
	var first, second float64
	var foundFirst, foundSecond bool
	firstIsDay := wp.Periods[0].IsDaytime
	for _, period := range wp.Periods {
		if period.IsDaytime == firstIsDay && !foundFirst {
			first = period.Temperature
			foundFirst = true
		} else if period.IsDaytime != firstIsDay && !foundSecond {
			second = period.Temperature
			foundSecond = true
		}
		if foundFirst && foundSecond {
			break
		}
	}
	if firstIsDay {
		switch {
		case foundFirst && foundSecond:
			return fmt.Sprintf("hi %d lo %d", int(first), int(second)), true
		case foundFirst:
			return fmt.Sprintf("hi %d", int(first)), true
		}
	} else {
		switch {
		case foundFirst && foundSecond:
			return fmt.Sprintf("lo %d hi %d", int(first), int(second)), true
		case foundFirst:
			return fmt.Sprintf("lo %d", int(first)), true
		}
	}
	return "", false
}

// formatCountdown formats a label + remaining time until target as a compact string.
// tideFeetInches formats a decimal-feet tide value as feet and inches,
// treating the tenths digit directly as inches (e.g. 7.5 → 7'5").
func tideFeetInches(v float64) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	ft := int(v)
	in := int(math.Round((v - float64(ft)) * 10))
	if in == 10 {
		ft++
		in = 0
	}
	return fmt.Sprintf("%s%d'%d\"", sign, ft, in)
}

func formatMoonCountdown(label string, target time.Time) string {
	rem := time.Until(target)
	if rem <= 0 {
		return label
	}
	hours := int(rem.Hours())
	if hours >= 24 {
		return fmt.Sprintf("%s %dd", label, hours/24)
	}
	return fmt.Sprintf("%s %dh", label, hours)
}

func formatCountdown(label string, target time.Time) string {
	rem := time.Until(target)
	if rem <= 0 {
		return label
	}
	totalMin := int(rem.Minutes())
	days := totalMin / (24 * 60)
	hours := (totalMin % (24 * 60)) / 60
	mins := totalMin % 60
	sep := " "
	if label == "" {
		sep = ""
	}
	if days > 0 {
		return fmt.Sprintf("%s%s%dd%dh", label, sep, days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%s%s%dh%dm", label, sep, hours, mins)
	}
	return fmt.Sprintf("%s%s%dm", label, sep, mins)
}

func moonPhaseIconPath(name string) string {
	switch name {
	case "Full Moon":
		return "resources/moon-9x7-full.png"
	case "New Moon":
		return "resources/moon-9x7-new.png"
	case "First Quarter":
		return "resources/moon-9x7-first_quarter.png"
	case "Last Quarter":
		return "resources/moon-9x7-last_quarter.png"
	case "Waxing Crescent":
		return "resources/moon-9x7-waxing_crescent.png"
	case "Waxing Gibbous":
		return "resources/moon-9x7-waxing_gibbous.png"
	case "Waning Gibbous":
		return "resources/moon-9x7-waning_gibbous.png"
	case "Waning Crescent":
		return "resources/moon-9x7-waning_crescent.png"
	default:
		return ""
	}
}

func loadIcon(path string) image.Image {
	if path == "" {
		return nil
	}
	data, err := resources.ReadFile(path)
	if err != nil {
		return nil
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	return img
}

func shortMoonLabel(name string) string {
	switch name {
	case "Full Moon":
		return "full"
	case "New Moon":
		return "new"
	default:
		if len(name) > 4 {
			return strings.ToLower(name[:4])
		}
		return strings.ToLower(name)
	}
}

func (p *RGBMatrixPlugin) getTempColor(status string) color.RGBA {
	switch status {
	case "warning":
		return p.colorRGBA(255, 200, 0, 255)
	case "critical":
		return p.colorRGBA(255, 0, 0, 255)
	case "ok":
		return p.colorRGBA(0, 100, 0, 255)
	case "stale":
		return p.colorRGBA(80, 80, 80, 255)
	default:
		return p.colorRGBA(50, 50, 50, 255)
	}
}

func (p *RGBMatrixPlugin) getTempColorWithStaleness(status string, lastUpdate time.Time) color.RGBA {
	// Check if data is stale (no update in 15+ minutes)
	if !lastUpdate.IsZero() && time.Since(lastUpdate) >= 15*time.Minute {
		return p.colorRGBA(80, 80, 80, 255)
	}
	return p.getTempColor(status)
}

// ValidateConfig validates the service configuration and returns any validation errors.
func (p *RGBMatrixPlugin) ValidateConfig() []error {
	// Optional subscriptions — store channel names when configured
	if moonSub, ok := p.cfg.Subs["moon"]; ok {
		p.moonChannelName = moonSub.Name
	}
	if sunSub, ok := p.cfg.Subs["sun"]; ok {
		p.sunChannelName = sunSub.Name
	}
	if weatherSub, ok := p.cfg.Subs["weather"]; ok {
		p.weatherChannelName = weatherSub.Name
	}
	if weatherReqPub, ok := p.cfg.Pubs["weather-request"]; ok {
		p.weatherRequestChannelName = weatherReqPub.Name
	}
	if tidesSub, ok := p.cfg.Subs["tides"]; ok {
		p.tidesChannelName = tidesSub.Name
	}
	if auroraSub, ok := p.cfg.Subs["aurora"]; ok {
		p.auroraChannelName = auroraSub.Name
	}
	return nil
}

// NewService creates a new service using the provided dependencies and configuration.
func NewService(deps core.Dependencies, cfg core.ServiceConfig, ctx context.Context) core.Service {

	return &RGBMatrixPlugin{
		deps: deps,
		cfg:  cfg,
		ctx:  ctx,
	}
}
