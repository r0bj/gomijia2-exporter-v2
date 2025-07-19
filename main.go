package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"
	"tinygo.org/x/bluetooth"
)

const (
	ver string = "0.34"
)

var adapter = bluetooth.DefaultAdapter

// Discovered device tracking
type DiscoveredDevice struct {
	Device   Device
	Address  bluetooth.Address
	LastSeen time.Time
}

// Command line flags
var (
	configFile      = pflag.StringP("config-file", "c", "config.ini", "Config file location")
	listenAddress   = pflag.StringP("web.listen-address", "l", ":8080", "Address to listen on for web interface and telemetry")
	readingInterval = pflag.IntP("reading-interval", "i", 60, "Interval between sensor readings in seconds")
	maxConcurrent   = pflag.IntP("max-concurrent", "m", 3, "Maximum concurrent BLE connections")
	debug           = pflag.BoolP("debug", "d", false, "Enable debug logging")
)

// Prometheus metrics
var (
	temperature = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mi_temperature",
		Help: "MI sensor temperature in Celsius",
	}, []string{"location"})

	humidity = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mi_humidity",
		Help: "MI sensor humidity percentage",
	}, []string{"location"})

	voltage = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mi_voltage",
		Help: "MI sensor battery voltage",
	}, []string{"location"})

	battery = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mi_battery",
		Help: "MI sensor battery level percentage",
	}, []string{"location"})

	deviceErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mi_device_errors_total",
		Help: "Total number of MI device errors",
	}, []string{"location"})
)

var (
	// Custom service UUID for Xiaomi Mijia 2
	customSvcUUID = bluetooth.NewUUID([16]byte{0xeb, 0xe0, 0xcc, 0xb0, 0x7a, 0x0a, 0x4b, 0x0c, 0x8a, 0x1a, 0x6f, 0xf2, 0x99, 0x7d, 0xa3, 0xa6})
	// Custom characteristic UUID for temperature/humidity data
	tempHumidityUUID = bluetooth.NewUUID([16]byte{0xeb, 0xe0, 0xcc, 0xc1, 0x7a, 0x0a, 0x4b, 0x0c, 0x8a, 0x1a, 0x6f, 0xf2, 0x99, 0x7d, 0xa3, 0xa6})
)

func main() {
	pflag.Parse()

	// Configure logging level
	if *debug {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	slog.Info("Starting", "version", ver)

	if err := adapter.Enable(); err != nil {
		slog.Error("Failed to enable bluetooth adapter", "error", err)
		return
	}

	// Load configuration
	config, err := NewConfig(*configFile)
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		return
	}

	if len(config.Devices) == 0 {
		slog.Error("No devices found in config file")
		return
	}

	slog.Info("Application started",
		"devices", len(config.Devices),
		"readingInterval", fmt.Sprintf("%ds", *readingInterval),
		"maxConcurrent", *maxConcurrent)

	// Initialize metrics for all devices
	for _, device := range config.Devices {
		// Initialize error counter to 0 for each device
		deviceErrors.WithLabelValues(device.Name).Add(0)
		slog.Debug("Initialized metrics for device", "device", device.Name)
	}

	// Start HTTP server for metrics
	go func() {
		slog.Info("Starting HTTP server for metrics", "address", *listenAddress)
		http.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(*listenAddress, nil); err != nil {
			slog.Error("HTTP server error", "error", err)
		}
	}()

	// Start the main collection loop
	runCollectionLoop(config.Devices)
}

func runCollectionLoop(devices []Device) {
	for {
		initialGoroutines := runtime.NumGoroutine()
		slog.Info("Starting discovery and collection cycle")
		slog.Debug("Goroutine count", "goroutines", initialGoroutines)

		// Phase 1: Discover all devices
		discovered := discoverDevices(devices)
		afterDiscovery := runtime.NumGoroutine()
		slog.Info("Discovery complete", "found", len(discovered), "total", len(devices))
		slog.Debug("Goroutine count after discovery", "goroutines", afterDiscovery)

		if len(discovered) == 0 {
			slog.Warn("No devices discovered, waiting before retry")
			time.Sleep(30 * time.Second)
			continue
		}

		// Phase 2: Connect to all discovered devices in parallel
		readAllDevices(discovered)
		afterReading := runtime.NumGoroutine()

		// Wait before next collection cycle
		slog.Info("Collection cycle complete, waiting for next cycle", "interval", *readingInterval)
		slog.Debug("Goroutine count after reading", "goroutines", afterReading)
		time.Sleep(time.Duration(*readingInterval) * time.Second)
	}
}

func discoverDevices(devices []Device) []DiscoveredDevice {
	slog.Info("Starting device discovery", "targets", len(devices))

	var mutex sync.Mutex

	// Create a map for quick lookup of target devices
	deviceMap := make(map[string]Device)
	for _, device := range devices {
		deviceMap[strings.ToLower(device.Addr)] = device
	}

	// Use a map to deduplicate discovered devices
	discoveredMap := make(map[string]DiscoveredDevice)

	// Scan for devices
	scanTimeout := 15 * time.Second
	scanCtx, scanCancel := context.WithTimeout(context.Background(), scanTimeout)
	defer scanCancel()

	// Channel to signal scan completion
	scanDone := make(chan struct{})
	var scanErr error

	go func() {
		defer close(scanDone)

		scanErr = adapter.Scan(func(_ *bluetooth.Adapter, res bluetooth.ScanResult) {
			// Check if context is cancelled
			select {
			case <-scanCtx.Done():
				return
			default:
			}

			addr := strings.ToLower(res.Address.String())
			if device, exists := deviceMap[addr]; exists {
				mutex.Lock()
				// Only add if not already discovered
				if _, alreadyDiscovered := discoveredMap[addr]; !alreadyDiscovered {
					discoveredMap[addr] = DiscoveredDevice{
						Device:   device,
						Address:  res.Address,
						LastSeen: time.Now(),
					}
					slog.Info("Discovered device", "name", device.Name, "address", res.Address.String())
				} else {
					// Just update the last seen time
					discovered := discoveredMap[addr]
					discovered.LastSeen = time.Now()
					discoveredMap[addr] = discovered
				}
				mutex.Unlock()
			}
		})
	}()

	// Wait for scan completion or timeout
	select {
	case <-scanDone:
		slog.Debug("Scan completed normally")
	case <-scanCtx.Done():
		slog.Debug("Scan timeout reached")
	}

	// Always stop scan to clean up resources
	if stopErr := adapter.StopScan(); stopErr != nil {
		slog.Debug("Error stopping scan", "error", stopErr)
	}

	// Log scan errors if any
	if scanErr != nil {
		slog.Error("Scan error", "error", scanErr)
	}

	// Convert map to slice
	var discovered []DiscoveredDevice
	for _, device := range discoveredMap {
		discovered = append(discovered, device)
	}

	// Check for devices that were expected but not discovered and increment error counter
	for _, expectedDevice := range devices {
		addr := strings.ToLower(expectedDevice.Addr)
		if _, found := discoveredMap[addr]; !found {
			deviceErrors.WithLabelValues(expectedDevice.Name).Inc()
			slog.Warn("Device not discovered during scan", "device", expectedDevice.Name, "address", expectedDevice.Addr)
		}
	}

	return discovered
}

func readAllDevices(discovered []DiscoveredDevice) {
	slog.Info("Starting parallel device reading", "devices", len(discovered))

	// Create a semaphore to limit concurrent connections
	sem := make(chan struct{}, *maxConcurrent)
	var wg sync.WaitGroup

	for _, dev := range discovered {
		wg.Add(1)

		go func(device DiscoveredDevice) {
			defer wg.Done()

			// Acquire semaphore
			sem <- struct{}{}
			defer func() { <-sem }()

			success := connectAndReadDevice(device)
			if !success {
				deviceErrors.WithLabelValues(device.Device.Name).Inc()
				slog.Warn("Failed to read device", "device", device.Device.Name)
			} else {
				slog.Debug("Successfully read device", "device", device.Device.Name)
			}
		}(dev)
	}

	wg.Wait()
	slog.Info("Parallel device reading complete")
}

func connectAndReadDevice(discovered DiscoveredDevice) bool {
	device := discovered.Device
	address := discovered.Address

	slog.Debug("Connecting to device", "device", device.Name, "address", address.String())

	// Set up overall operation timeout
	operationTimeout := 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()

	// Channel to receive operation result
	resultChan := make(chan bool, 1)

	// Run the connection and read operation
	go func() {
		defer func() {
			// Ensure we always send a result, even if panics occur
			select {
			case resultChan <- performDeviceRead(ctx, device, address):
			case <-ctx.Done():
				// Context cancelled, don't block
			}
		}()
	}()

	// Wait for operation completion or timeout
	select {
	case result := <-resultChan:
		return result
	case <-ctx.Done():
		slog.Warn("Operation timeout", "device", device.Name, "timeout", operationTimeout)
		return false
	}
}

func performDeviceRead(ctx context.Context, device Device, address bluetooth.Address) bool {
	// Check if context is already cancelled
	select {
	case <-ctx.Done():
		slog.Debug("Context cancelled before starting", "device", device.Name)
		return false
	default:
	}

	// Connect to the device
	dev, err := adapter.Connect(address, bluetooth.ConnectionParams{})
	if err != nil {
		slog.Error("Failed to connect to device", "device", device.Name, "error", err)
		return false
	}
	defer func() {
		if disconnectErr := dev.Disconnect(); disconnectErr != nil {
			slog.Debug("Error disconnecting from device", "device", device.Name, "error", disconnectErr)
		}
	}()

	// Check context after connection
	select {
	case <-ctx.Done():
		slog.Debug("Context cancelled after connect", "device", device.Name)
		return false
	default:
	}

	slog.Debug("Connected, discovering GATT services", "device", device.Name)
	svcs, err := dev.DiscoverServices([]bluetooth.UUID{customSvcUUID})
	if err != nil {
		slog.Error("Failed to discover services", "device", device.Name, "error", err)
		return false
	}
	if len(svcs) == 0 {
		slog.Error("Custom Xiaomi service not found", "device", device.Name)
		return false
	}

	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{tempHumidityUUID})
	if err != nil {
		slog.Error("Failed to discover characteristics", "device", device.Name, "error", err)
		return false
	}
	if len(chars) == 0 {
		slog.Error("Temperature/humidity characteristic missing", "device", device.Name)
		return false
	}
	tempChar := chars[0]

	// Check context before notifications
	select {
	case <-ctx.Done():
		slog.Debug("Context cancelled before notifications", "device", device.Name)
		return false
	default:
	}

	slog.Debug("Setting up notifications", "device", device.Name)

	// Channel to receive notification data
	dataChan := make(chan []byte, 1)
	var notificationEnabled bool

	// Enable notifications (required for Xiaomi Mijia 2 devices)
	err = tempChar.EnableNotifications(func(buf []byte) {
		select {
		case dataChan <- buf:
		case <-ctx.Done():
			// Context cancelled, don't block
		default:
			// Channel might be full or closed, don't block
		}
	})

	if err != nil {
		slog.Error("Failed to enable notifications", "device", device.Name, "error", err)
		return false
	}
	notificationEnabled = true

	// Ensure notifications are disabled when we're done
	defer func() {
		if notificationEnabled {
			if disableErr := tempChar.EnableNotifications(nil); disableErr != nil {
				slog.Debug("Failed to disable notifications", "device", device.Name, "error", disableErr)
			}
		}
	}()

	slog.Debug("Notifications enabled, waiting for data", "device", device.Name)

	// Wait for data with context cancellation
	select {
	case data := <-dataChan:
		if len(data) != 5 {
			slog.Error("Unexpected data length", "device", device.Name, "length", len(data), "expected", 5)
			return false
		}

		// Parse data according to old version format: T2 T1 HX V1 V2
		temp := float32(int16(binary.LittleEndian.Uint16(data[0:2]))) / 100.0
		hum := float32(data[2])
		volt := float32(int16(binary.LittleEndian.Uint16(data[3:5]))) / 1000.0

		// Calculate battery percentage: 3.1V or above = 100%, 2.1V = 0%
		batteryPercent := math.Round(math.Min((float64(volt)-2.1)*100, 100)*100) / 100

		// Update Prometheus metrics
		temperature.WithLabelValues(device.Name).Set(float64(temp))
		humidity.WithLabelValues(device.Name).Set(float64(hum))
		voltage.WithLabelValues(device.Name).Set(float64(volt))
		battery.WithLabelValues(device.Name).Set(batteryPercent)

		// Log sensor data with structured logging
		slog.Info("Sensor data",
			"device", device.Name,
			"temperature", fmt.Sprintf("%.2f", temp),
			"humidity", fmt.Sprintf("%.0f", hum),
			"voltage", fmt.Sprintf("%.3f", volt),
			"battery", fmt.Sprintf("%.1f", batteryPercent),
			"timestamp", time.Now().Format(time.RFC3339))

		slog.Debug("Sensor data received, disconnecting", "device", device.Name)
		return true

	case <-time.After(6 * time.Second):
		slog.Warn("Timeout waiting for sensor data", "device", device.Name)
		return false
	case <-ctx.Done():
		slog.Debug("Context cancelled while waiting for data", "device", device.Name)
		return false
	}
}
