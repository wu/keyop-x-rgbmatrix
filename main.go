package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"keyop/core"
	"os"
	"strings"
	"time"

	"context"
	"sort"
	"sync"

	"github.com/mcuadros/go-rpi-rgb-led-matrix"
	"github.com/wu/keyop-messenger"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

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

	mu        sync.RWMutex
	temps     map[string]float64
	statuses  map[string]string
	tempNames []string
	tempIdx   int
	nameMap   map[string]string
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

	m, err := rgbmatrix.NewRGBLedMatrix(config)
	if err != nil {
		return fmt.Errorf("failed to initialize matrix: %w", err)
	}

	p.matrix = m
	p.canvas = rgbmatrix.NewCanvas(p.matrix)
	p.temps = make(map[string]float64)
	p.statuses = make(map[string]string)
	p.nameMap = make(map[string]string)

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
	fontBytes, err := os.ReadFile(path) //nolint:gosec // reading bundled resource file path
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

// Check performs the service's periodic work: collect data, evaluate state, and publish messages/metrics.
func (p *RGBMatrixPlugin) Check() error {
	logger := p.deps.MustGetLogger()
	logger.Info("RGBMatrixPlugin Check called")

	newMsgr := p.deps.MustGetMessenger()
	if newMsgr == nil {
		return fmt.Errorf("rgbmatrix: new messenger not initialized")
	}

	// Register payload types
	if err := newMsgr.RegisterPayloadType("core.temp.v1", &core.TempEvent{}); err != nil {
		if !core.IsDuplicatePayloadRegistration(err) {
			logger.Error("rgbmatrix: failed to register core.temp.v1", "error", err)
			return err
		}
	}

	// Subscribe to temp channel
	subscriberID := "rgbmatrix-temp"
	logger.Info("rgbmatrix: subscribing to temp channel", "subscriberID", subscriberID)
	if err := newMsgr.Subscribe(p.ctx, "temp", subscriberID, func(msgCtx context.Context, msg messenger.Message) error {
		p.mu.Lock()
		defer p.mu.Unlock()

		// Decode the temperature event
		var tempEvent *core.TempEvent
		if te, ok := msg.Payload.(*core.TempEvent); ok {
			tempEvent = te
		} else if te, ok := msg.Payload.(core.TempEvent); ok {
			tempEvent = &te
		} else {
			logger.Info("rgbmatrix: received non-TempEvent message", "payloadType", msg.PayloadType, "payloadType_actual", fmt.Sprintf("%T", msg.Payload))
			return nil
		}

		// Use sensor name if available, otherwise use origin
		sensorName := tempEvent.SensorName
		if sensorName == "" {
			sensorName = msg.Origin
		}

		p.temps[sensorName] = float64(tempEvent.TempF)
		p.statuses[sensorName] = "ok" // Default status

		// Update sorted names
		names := make([]string, 0, len(p.temps))
		for name := range p.temps {
			names = append(names, name)
		}
		sort.Strings(names)
		p.tempNames = names

		logger.Info("Got temp update", "sensorName", sensorName, "tempF", tempEvent.TempF, "totalTemps", len(p.temps))
		return nil
	}); err != nil {
		logger.Error("rgbmatrix: failed to subscribe to temp channel", "error", err)
		return fmt.Errorf("rgbmatrix: failed to subscribe to temp channel: %w", err)
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	cycleTicker := time.NewTicker(3 * time.Second)
	defer cycleTicker.Stop()

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
		if p.tempNames[p.tempIdx] == "outdoor" {
			// Skip outside temp in cycle since it's always shown as main temp
			p.tempIdx = (p.tempIdx + 1) % len(p.tempNames)
		}
		name := p.tempNames[p.tempIdx]
		temp := p.temps[name]

		// todo: main temp should be configurable
		mainTemp, ok := p.temps["outdoor"]
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
		d.Src = image.NewUniform(p.getTempColor(p.statuses["outdoor"]))
		d.Dot = fixed.Point26_6{X: fixed.I(0), Y: fixed.I(20)}
		d.DrawString(mainTempStr)

		tempStr := fmt.Sprintf("%.1f", temp)
		d.Face = p.bigFace
		d.Src = image.NewUniform(p.getTempColor(p.statuses[name]))
		d.Dot = fixed.Point26_6{X: fixed.I(26), Y: fixed.I(20)}
		d.DrawString(tempStr)

		tempNameStr := fmt.Sprintf("%s", displayName)
		d.Face = p.bigFace
		d.Src = image.NewUniform(p.getTempColor(p.statuses[name]))
		d.Dot = fixed.Point26_6{X: fixed.I(52), Y: fixed.I(20)}
		d.DrawString(tempNameStr)
	}

	//// 5. Draw Info (Bottom)
	//infoStr := "keyop"
	//d.Face = p.bigFace
	//d.Src = image.NewUniform(p.colorRGBA(0, 100, 0, 255))
	//d.Dot = fixed.Point26_6{X: fixed.I(0), Y: fixed.I(30)}
	//d.DrawString(infoStr)

	p.mu.RUnlock()

	// Copy image pixels to canvas
	draw.Draw(p.canvas, bounds, img, image.Point{}, draw.Over)

	p.canvas.Render()

	return nil
}

func (p *RGBMatrixPlugin) getTempColor(status string) color.RGBA {
	switch status {
	case "warning":
		return p.colorRGBA(50, 50, 255, 255)
	case "critical":
		return p.colorRGBA(255, 0, 0, 255)
	case "ok":
		return p.colorRGBA(0, 100, 0, 255)
	default:
		return p.colorRGBA(50, 50, 50, 255)
	}
}

// ValidateConfig validates the service configuration and returns any validation errors.
func (p *RGBMatrixPlugin) ValidateConfig() []error {
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
