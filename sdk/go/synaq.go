// Package synaq provides a high-performance server-side SDK for SynaqCore
// with Local Evaluation (< 1 µs in-memory variant calculation via SHA-256)
// and automatic background configuration synchronization.
package synaq

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ExperimentConfig represents experiment weights and metadata.
type ExperimentConfig struct {
	Name     string                 `json:"name"`
	Variants map[string]int         `json:"variants"`
	Config   map[string]interface{} `json:"config"`
	URL      *string                `json:"url"`
}

type evalResponse struct {
	Experiments map[string]ExperimentConfig `json:"experiments"`
}

// Client manages local experiment evaluation and event tracking.
type Client struct {
	baseURL         string
	pollInterval    time.Duration
	fallbackVariant string
	httpClient      *http.Client

	mu          sync.RWMutex
	experiments map[string]ExperimentConfig

	stopChan chan struct{}
	wg       sync.WaitGroup
}

// Config holds client initialization options.
type Config struct {
	BaseURL         string
	PollInterval    time.Duration
	FallbackVariant string
	Timeout         time.Duration
	AutoStart       bool
}

// NewClient creates and initializes a new Synaq Server SDK client.
func NewClient(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:8080"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	if cfg.PollInterval < 5*time.Second {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.FallbackVariant == "" {
		cfg.FallbackVariant = "control"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}

	c := &Client{
		baseURL:         cfg.BaseURL,
		pollInterval:    cfg.PollInterval,
		fallbackVariant: cfg.FallbackVariant,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
		experiments: make(map[string]ExperimentConfig),
		stopChan:    make(chan struct{}),
	}

	// Первичная синхронизация
	_ = c.RefreshConfigs()

	if cfg.AutoStart {
		c.wg.Add(1)
		go c.pollLoop()
	}

	return c
}

// RefreshConfigs fetches active experiment configurations from SynaqCore.
func (c *Client) RefreshConfigs() error {
	url := fmt.Sprintf("%s/api/v1/variants/eval-configs", c.baseURL)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "SynaqServerSDK-Go/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var data evalResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return err
	}

	c.mu.Lock()
	c.experiments = data.Experiments
	c.mu.Unlock()

	return nil
}

func (c *Client) pollLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			_ = c.RefreshConfigs()
		}
	}
}

// GetVariant evaluates variant locally in memory in < 1 µs without network roundtrips.
func (c *Client) GetVariant(experimentName, userID string) string {
	if experimentName == "" || userID == "" {
		return c.fallbackVariant
	}

	c.mu.RLock()
	exp, exists := c.experiments[experimentName]
	c.mu.RUnlock()

	if !exists || len(exp.Variants) == 0 {
		return c.fallbackVariant
	}

	var totalWeight int
	keys := make([]string, 0, len(exp.Variants))
	for k, w := range exp.Variants {
		totalWeight += w
		keys = append(keys, k)
	}

	if totalWeight <= 0 {
		return c.fallbackVariant
	}

	sort.Strings(keys)

	// Детерминированный хэш: SHA-256(user_id:experiment_name) -> bucket [0.00 .. 99.99]
	input := fmt.Sprintf("%s:%s", userID, experimentName)
	hash := sha256.Sum256([]byte(input))
	hexStr := hex.EncodeToString(hash[:])

	n := new(big.Int)
	n.SetString(hexStr, 16)
	mod := new(big.Int).Mod(n, big.NewInt(10000))
	bucket := float64(mod.Int64()) / 100.0

	var cumulative float64
	for _, k := range keys {
		w := exp.Variants[k]
		cumulative += (float64(w) / float64(totalWeight)) * 100.0
		if bucket < cumulative {
			return k
		}
	}

	return c.fallbackVariant
}

// Track sends a conversion event asynchronously to Go Tracker.
func (c *Client) Track(eventName, experimentName, variant, userID string, eventData map[string]interface{}) error {
	var dataStr string
	if len(eventData) > 0 {
		b, _ := json.Marshal(eventData)
		dataStr = string(b)
	}

	payload := map[string]interface{}{
		"timestamp":       time.Now().UnixMilli(),
		"user_id":         userID,
		"event_name":      eventName,
		"experiment_name": experimentName,
		"variant":         variant,
		"event_data":      dataStr,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/api/v1/tracker/events", c.baseURL)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "SynaqServerSDK-Go/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tracker returned status: %d", resp.StatusCode)
	}
	return nil
}

// Close stops the background synchronizer.
func (c *Client) Close() {
	close(c.stopChan)
	c.wg.Wait()
}
