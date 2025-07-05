package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"tinygo.org/x/bluetooth"
)

const (
	ver string = "0.1"
)

var adapter = bluetooth.DefaultAdapter

// Mutex to synchronize access to the bluetooth adapter
var adapterMutex sync.Mutex

// Command line flags
var (
	configFile      = flag.String("config-file", "config.ini", "Config file location")
	listenAddress   = flag.String("web.listen-address", ":8080", "Address to listen on for web interface and telemetry")
	readingInterval = flag.Int("reading-interval", 60, "Interval between sensor readings in seconds")
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
	// Custom service UUID for Xiaomi Mijia 2 (based on working version)
	customSvcUUID = bluetooth.NewUUID([16]byte{0xeb, 0xe0, 0xcc, 0xb0, 0x7a, 0x0a, 0x4b, 0x0c, 0x8a, 0x1a, 0x6f, 0xf2, 0x99, 0x7d, 0xa3, 0xa6})
	// Custom characteristic UUID for temperature/humidity data
	tempHumidityUUID = bluetooth.NewUUID([16]byte{0xeb, 0xe0, 0xcc, 0xc1, 0x7a, 0x0a, 0x4b, 0x0c, 0x8a, 0x1a, 0x6f, 0xf2, 0x99, 0x7d, 0xa3, 0xa6})
)

func main() {
	flag.Parse()

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
		"readingInterval", fmt.Sprintf("%ds", *readingInterval))

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

	// Use WaitGroup to handle multiple devices
	var wg sync.WaitGroup

	// Start a goroutine for each device with staggered timing
	for i, device := range config.Devices {
		wg.Add(1)
		go func(dev Device, delay int) {
			defer wg.Done()
			// Stagger the start times to avoid immediate collisions
			time.Sleep(time.Duration(delay) * time.Second)
			handleDevice(dev)
		}(device, i*10) // 10 second delay between devices
	}

	// Wait for all devices to complete
	wg.Wait()
}

func handleDevice(device Device) {
	slog.Info("Starting device handler", "device", device.Name, "address", device.Addr)

	consecutiveFailures := 0
	for {
		success := connectAndReadDevice(device)
		if !success {
			consecutiveFailures++

			// Increment error counter
			deviceErrors.WithLabelValues(device.Name).Inc()

			// Use longer retry interval for devices that seem to be offline
			var retryInterval time.Duration
			if consecutiveFailures >= 3 {
				retryInterval = 120 * time.Second // 2 minutes for likely offline devices
				slog.Warn("Device appears to be offline, using longer retry interval",
					"device", device.Name,
					"consecutiveFailures", consecutiveFailures,
					"retryInterval", retryInterval)
			} else {
				retryInterval = 30 * time.Second // 30 seconds for temporary failures
				slog.Info("Device read failed, retrying",
					"device", device.Name,
					"retryInterval", retryInterval)
			}

			time.Sleep(retryInterval)
		} else {
			// Reset failure counter on successful read
			if consecutiveFailures > 0 {
				slog.Info("Device is back online",
					"device", device.Name,
					"previousFailures", consecutiveFailures)
				consecutiveFailures = 0
			}

			// Wait before next reading
			time.Sleep(time.Duration(*readingInterval) * time.Second)
		}
	}
}

func connectAndReadDevice(device Device) bool {
	// Lock the adapter for exclusive access
	slog.Debug("Waiting for adapter access", "device", device.Name)
	adapterMutex.Lock()
	defer adapterMutex.Unlock()

	slog.Debug("Acquired adapter, starting scan", "device", device.Name, "address", device.Addr)

	// Set up overall operation timeout
	operationTimeout := 45 * time.Second // Increased timeout for full operation
	operationDone := make(chan bool, 1)
	var operationResult bool

	// Run the entire operation with timeout
	go func() {
		result := performDeviceOperation(device)
		operationResult = result
		operationDone <- true
	}()

	// Wait for operation completion or timeout
	select {
	case <-operationDone:
		return operationResult
	case <-time.After(operationTimeout):
		slog.Warn("Operation timeout - device may be offline",
			"device", device.Name,
			"address", device.Addr,
			"timeout", operationTimeout)
		adapter.StopScan() // Stop any ongoing scan
		return false
	}
}

func performDeviceOperation(device Device) bool {
	var dev bluetooth.Device
	found := false

	// The Scan callback is called for every advertisement.
	err := adapter.Scan(func(_ *bluetooth.Adapter, res bluetooth.ScanResult) {
		if strings.EqualFold(res.Address.String(), device.Addr) {
			slog.Info("Found device, connecting", "device", device.Name, "address", res.Address.String())
			adapter.StopScan()
			var err error
			dev, err = adapter.Connect(res.Address, bluetooth.ConnectionParams{})
			if err != nil {
				slog.Error("Failed to connect to device", "device", device.Name, "error", err)
				return
			}
			found = true
		}
	})
	if err != nil {
		slog.Error("Scan error", "device", device.Name, "error", err)
		return false
	}

	// If Scan returned without having set 'found', nothing was discovered.
	if !found {
		slog.Debug("Device not discovered within scan window", "device", device.Name, "address", device.Addr)
		return false
	}
	defer dev.Disconnect()

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

	slog.Debug("Setting up notifications", "device", device.Name)

	// Channel to receive notification data
	dataChan := make(chan []byte, 1)
	dataReceived := false

	// Enable notifications (required for Xiaomi Mijia 2 devices)
	err = tempChar.EnableNotifications(func(buf []byte) {
		if !dataReceived {
			select {
			case dataChan <- buf:
				dataReceived = true
			default:
				// Channel full, drop the data
			}
		}
	})

	if err != nil {
		slog.Error("Failed to enable notifications", "device", device.Name, "error", err)
		return false
	}

	slog.Debug("Notifications enabled, waiting for data", "device", device.Name)

	// Wait for data
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

		slog.Debug("Sensor data received, disconnecting immediately", "device", device.Name)
		return true

	case <-time.After(6 * time.Second):
		slog.Warn("Timeout waiting for sensor data", "device", device.Name)
		return false
	}
}
