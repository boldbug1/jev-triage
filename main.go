package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

const model = "jev-latest"

func apiBase() string {
	if b := os.Getenv("TYPESAFE_API_BASE"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://api.typesafe.ai"
}

var urgencyLabels = []string{"not urgent", "low", "high", "critical"}

// ---------- Request / response shapes ----------

type Question struct {
	Type         string      `json:"type"`
	Instructions string      `json:"instructions"`
	Criteria     interface{} `json:"criteria,omitempty"`
}

type Request struct {
	Model     string              `json:"model"`
	State     interface{}         `json:"state"`
	Questions map[string]Question `json:"questions"`
}

type Answer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
}

// ---------- Talking to the API ----------

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("jev returned %d: %s", e.Status, e.Body)
}

func (e *apiError) retryable() bool {
	return e.Status == 429 || e.Status == 529 || e.Status >= 500
}

func evaluate(client *http.Client, apiKey, state string, questions map[string]Question) (*Response, error) {
	body, err := json.Marshal(Request{Model: model, State: state, Questions: questions})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, apiBase()+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}

	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func evaluateWithRetry(client *http.Client, apiKey, state string, questions map[string]Question) (*Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := evaluate(client, apiKey, state, questions)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		var ae *apiError
		if !errors.As(err, &ae) || !ae.retryable() {
			return nil, err
		}
		time.Sleep(time.Duration(1<<attempt) * time.Second)
	}
	return nil, lastErr
}

func buildQuestions() map[string]Question {
	return map[string]Question{
		"category": {
			Type:         "choice",
			Instructions: "Which kind of message is this?",
			Criteria: map[string]string{
				"bug":             "Something is broken or behaving incorrectly",
				"billing":         "Payments, invoices, refunds",
				"feature_request": "Asking for something new",
				"account":         "Login, password, permissions",
				"other":           "Anything that doesn't fit the above",
			},
		},
		"urgency": {
			Type:         "score",
			Instructions: "How urgent is this message?",
			Criteria: []string{
				"Not urgent: general question or idea",
				"Low: minor annoyance, has a workaround",
				"High: blocks the person from doing their work",
				"Critical: outage or money being lost right now",
			},
		},
		"frustrated": {
			Type:         "noul",
			Instructions: "Does the sender sound frustrated or angry?",
		},
	}
}

// ---------- Triage ----------

type Result struct {
	Message    string
	Category   string
	CatConf    float64
	Urgency    float64
	Frustrated float64
	Err        error
}

func triage(client *http.Client, apiKey, message string) Result {
	res := Result{Message: message}

	resp, err := evaluateWithRetry(client, apiKey, message, buildQuestions())
	if err != nil {
		res.Err = err
		return res
	}

	cat, ok1 := resp.Answers["category"]
	urg, ok2 := resp.Answers["urgency"]
	fr, ok3 := resp.Answers["frustrated"]
	if !ok1 || !ok2 || !ok3 {
		res.Err = errors.New("response is missing one of the answers")
		return res
	}

	res.Category = cat.Choice
	res.CatConf = cat.Confidence
	res.Urgency = urg.Score
	res.Frustrated = fr.Noul
	return res
}

func triageAll(client *http.Client, apiKey string, messages []string, workers int) []Result {
	results := make([]Result, len(messages))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, m := range messages {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, m string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = triage(client, apiKey, m)
		}(i, m)
	}
	wg.Wait()
	return results
}

func readMessages(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// ---------- Output helpers ----------

func level(score float64) int {
	i := int(math.Round(score))
	if i < 0 {
		i = 0
	}
	if i >= len(urgencyLabels) {
		i = len(urgencyLabels) - 1
	}
	return i
}

func bar(conf float64) string {
	n := int(math.Round(conf * 10))
	if n < 0 {
		n = 0
	}
	if n > 10 {
		n = 10
	}
	return strings.Repeat("#", n) + strings.Repeat(".", 10-n)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func printTable(title string, rows []Result) {
	fmt.Printf("\n%s (%d)\n", title, len(rows))
	if len(rows) == 0 {
		fmt.Println("  none")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "URGENCY\tCATEGORY\tCONFIDENCE\tFRUSTRATED\tMESSAGE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%.1f %s\t%s\t%s %.0f%%\t%.0f%%\t%s\n",
			r.Urgency, urgencyLabels[level(r.Urgency)],
			r.Category,
			bar(r.CatConf), r.CatConf*100,
			r.Frustrated*100,
			truncate(r.Message, 60))
	}
	tw.Flush()
}

// ---------- HTML report ----------

type row struct {
	Message    string
	Category   string
	Conf       int
	Urgency    string
	Label      string
	Level      int
	Frustrated int
	Review     bool
}

const pageTmpl = `<!doctype html>
<html><head><meta charset="utf-8"><title>Triage report</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 2rem auto; max-width: 1000px; padding: 0 1rem; color: #222; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid #ddd; vertical-align: top; }
  th { background: #f4f4f4; }
  .l0 { border-left: 6px solid #9aa5b1; }
  .l1 { border-left: 6px solid #e5c100; }
  .l2 { border-left: 6px solid #f08c00; }
  .l3 { border-left: 6px solid #d6336c; background: #fff0f3; }
  .track { background: #e6e6e6; border-radius: 4px; width: 90px; height: 10px; display: inline-block; }
  .fill { background: #3b82f6; height: 10px; border-radius: 4px; display: block; }
  .low .fill { background: #f08c00; }
  .tag { font-size: 0.8em; background: #fff3bf; border-radius: 4px; padding: 1px 6px; margin-left: 6px; }
  .err { color: #b00020; }
</style></head><body>
<h1>Triage report</h1>
<p>{{.Total}} messages, sorted by urgency. {{.ReviewCount}} need a human look (low confidence). {{len .Failed}} failed.</p>
<table>
<tr><th>Urgency</th><th>Category</th><th>Confidence</th><th>Frustrated</th><th>Message</th></tr>
{{range .Rows}}
<tr class="l{{.Level}}">
  <td>{{.Urgency}} {{.Label}}</td>
  <td>{{.Category}}{{if .Review}}<span class="tag">review</span>{{end}}</td>
  <td><span class="track {{if .Review}}low{{end}}"><span class="fill" style="width:{{.Conf}}%"></span></span> {{.Conf}}%</td>
  <td>{{.Frustrated}}%</td>
  <td>{{.Message}}</td>
</tr>
{{end}}
</table>
{{if .Failed}}
<h2>Failed</h2>
<ul>{{range .Failed}}<li class="err">{{.Message}} — {{.Error}}</li>{{end}}</ul>
{{end}}
</body></html>`

type failedRow struct {
	Message string
	Error   string
}

func writeHTML(path string, ok []Result, failed []Result, threshold float64) error {
	var rows []row
	review := 0
	for _, r := range ok {
		needs := r.CatConf < threshold
		if needs {
			review++
		}
		rows = append(rows, row{
			Message:    r.Message,
			Category:   r.Category,
			Conf:       int(math.Round(r.CatConf * 100)),
			Urgency:    fmt.Sprintf("%.1f", r.Urgency),
			Label:      urgencyLabels[level(r.Urgency)],
			Level:      level(r.Urgency),
			Frustrated: int(math.Round(r.Frustrated * 100)),
			Review:     needs,
		})
	}
	var fails []failedRow
	for _, f := range failed {
		fails = append(fails, failedRow{Message: f.Message, Error: f.Err.Error()})
	}

	t, err := template.New("page").Parse(pageTmpl)
	if err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	return t.Execute(out, map[string]interface{}{
		"Rows":        rows,
		"Failed":      fails,
		"Total":       len(ok) + len(failed),
		"ReviewCount": review,
	})
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff") 
			first = false
		}
		line = strings.TrimSpace(line) 
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)

		if key != "" && os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}

// ---------- Main ----------

func main() {
	loadDotEnv(".env")

	file := flag.String("file", "messages.txt", "text file with one message per line")
	htmlOut := flag.String("html", "report.html", "where to write the HTML report ('' to skip)")
	threshold := flag.Float64("threshold", 0.8, "category confidence below this goes to the review pile")
	workers := flag.Int("workers", 4, "how many requests to run at the same time")
	flag.Parse()

	apiKey := os.Getenv("TYPESAFE_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "set TYPESAFE_API_KEY first")
		os.Exit(1)
	}

	messages, err := readMessages(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not read messages:", err)
		os.Exit(1)
	}
	if len(messages) == 0 {
		fmt.Fprintln(os.Stderr, "no messages found in", *file)
		os.Exit(1)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	fmt.Printf("triaging %d messages...\n", len(messages))
	results := triageAll(client, apiKey, messages, *workers)

	var ok, failed, review []Result
	for _, r := range results {
		switch {
		case r.Err != nil:
			failed = append(failed, r)
		default:
			ok = append(ok, r)
			if r.CatConf < *threshold {
				review = append(review, r)
			}
		}
	}

	sort.SliceStable(ok, func(i, j int) bool { return ok[i].Urgency > ok[j].Urgency })

	printTable("All messages, most urgent first", ok)
	printTable(fmt.Sprintf("Review by hand (category confidence below %.0f%%)", *threshold*100), review)

	if len(failed) > 0 {
		fmt.Printf("\nFailed (%d)\n", len(failed))
		for _, f := range failed {
			fmt.Printf("  %s\n    -> %v\n", truncate(f.Message, 60), f.Err)
		}
	}

	if *htmlOut != "" {
		if err := writeHTML(*htmlOut, ok, failed, *threshold); err != nil {
			fmt.Fprintln(os.Stderr, "could not write report:", err)
			os.Exit(1)
		}
		fmt.Printf("\nwrote %s\n", *htmlOut)
	}
}
