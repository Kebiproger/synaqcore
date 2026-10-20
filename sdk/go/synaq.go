// Package synaq provides a high-performance server-side SDK for SynaqCore
// with Local Evaluation (< 1 µs in-memory variant calculation via SHA-256)
// and automatic background configuration synchronization.
package synaq

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
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

// allocationBuckets — число бакетов распределения (шаг 0.01%).
const allocationBuckets = 10000

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

	return allocate(userID, experimentName, exp.Variants, c.fallbackVariant)
}

// allocate — детерминированный выбор варианта. Алгоритм обязан совпадать с
// get_user_variant() в experiment_api (apps/experiment_api/src/services/allocation.py)
// и с Python SDK; паритет закреплён golden-векторами в synaq_test.go.
//
// bucket = SHA-256(user_id:experiment_name) mod 10000 (0..9999). Варианты
// перебираются по имени в алфавитном порядке (sort.Strings == Python sorted()
// для ASCII-имён): порядок ключей JSON не гарантирован. Вариант выбирается,
// если bucket/10000 < cumulative/total; сравнение в целых числах
// (bucket*total < cumulative*10000, 128-битное умножение), без float.
func allocate(userID, experimentName string, variants map[string]int, fallback string) string {
	if len(variants) == 0 {
		return fallback
	}

	var total uint64
	keys := make([]string, 0, len(variants))
	for k, w := range variants {
		// Неположительные веса сервер не принимает; сумма не должна переполняться.
		if w <= 0 || uint64(w) > math.MaxUint64-total {
			return fallback
		}
		total += uint64(w)
		keys = append(keys, k)
	}
	sort.Strings(keys)

	sum := sha256.Sum256([]byte(userID + ":" + experimentName))
	// mod 10000 по big-endian байтам хэша == int(hash_hex, 16) % 10000 в Python.
	var bucket uint64
	for _, b := range sum {
		bucket = (bucket*256 + uint64(b)) % allocationBuckets
	}

	lhsHi, lhsLo := bits.Mul64(bucket, total)
	var cumulative uint64
	for _, k := range keys {
		cumulative += uint64(variants[k])
		rhsHi, rhsLo := bits.Mul64(cumulative, allocationBuckets)
		if lhsHi < rhsHi || (lhsHi == rhsHi && lhsLo < rhsLo) {
			return k
		}
	}

	return fallback
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
