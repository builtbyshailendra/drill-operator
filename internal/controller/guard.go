package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	drillsv1 "github.com/builtbyshailendra/drill-operator/api/v1alpha1"
)

// GuardReader reads the current value of a Prometheus guard.
type GuardReader interface {
	Read(ctx context.Context, g *drillsv1.PrometheusGuard) (float64, error)
}

// PrometheusReader runs an instant query against the Prometheus HTTP API.
type PrometheusReader struct {
	HTTP *http.Client
}

// Read expects exactly one number back. An empty result is an error, not
// zero: "no data" could mean the service stopped reporting, which is the
// moment a drill should stop. Write guard queries that always return a value
// (for an error ratio, append `or vector(0)`).
func (p PrometheusReader) Read(ctx context.Context, g *drillsv1.PrometheusGuard) (float64, error) {
	hc := p.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	u := strings.TrimRight(g.URL, "/") + "/api/v1/query?query=" + url.QueryEscape(g.Query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prometheus answered %s", resp.Status)
	}
	var r struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Status != "success" {
		return 0, fmt.Errorf("unexpected prometheus reply")
	}
	var sample []any
	switch r.Data.ResultType {
	case "scalar":
		if err := json.Unmarshal(r.Data.Result, &sample); err != nil {
			return 0, err
		}
	case "vector":
		var vec []struct {
			Value []any `json:"value"`
		}
		if err := json.Unmarshal(r.Data.Result, &vec); err != nil {
			return 0, err
		}
		if len(vec) != 1 {
			return 0, fmt.Errorf("query returned %d series, want exactly 1", len(vec))
		}
		sample = vec[0].Value
	default:
		return 0, fmt.Errorf("query returned a %s, want a vector or scalar", r.Data.ResultType)
	}
	if len(sample) != 2 {
		return 0, fmt.Errorf("malformed sample")
	}
	s, ok := sample[1].(string)
	if !ok {
		return 0, fmt.Errorf("malformed sample value")
	}
	return parseValue(s)
}

func parseValue(s string) (float64, error) { return strconv.ParseFloat(s, 64) }

func formatValue(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// above reports whether v is over the limit. NaN counts as over: an error
// ratio of NaN usually means zero traffic, which a drill should not ignore.
func above(v float64, limit string) (bool, error) {
	l, err := parseValue(limit)
	if err != nil {
		return true, err
	}
	return v != v || v > l, nil
}

var drillsFinished = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "drill_operator_drills_finished_total",
	Help: "Drills that reached a final phase.",
}, []string{"phase"})

func init() {
	metrics.Registry.MustRegister(drillsFinished)
}
