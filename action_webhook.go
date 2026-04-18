package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/template"
)

// WebhookExecutor sends custom HTTP requests with templated body.
type WebhookExecutor struct {
	client *http.Client
}

type webhookConfig struct {
	URL          string            `json:"url"`
	Method       string            `json:"method"`
	Headers      map[string]string `json:"headers"`
	BodyTemplate string            `json:"body_template"`
}

// WebhookTemplateData is the context passed to body templates.
// SourceName is kept as an alias for backward compatibility with existing templates.
type WebhookTemplateData struct {
	WANInterfaceName string
	SourceName       string // alias for WANInterfaceName (backward compat)
	NewIP            string
	OldIP            string
	Event            string
	Timestamp        string
}

// webhookFuncs provides template functions for safe JSON interpolation.
// Usage in body_template: {{json .WANInterfaceName}} outputs a JSON-escaped string (with quotes).
var webhookFuncs = template.FuncMap{
	"json": func(v interface{}) string {
		b, _ := json.Marshal(v)
		return string(b)
	},
}

func (e *WebhookExecutor) Type() string { return "webhook" }

func (e *WebhookExecutor) DisplayName() string { return "HTTP Webhook" }

func (e *WebhookExecutor) Description() string {
	return "POST (or PUT/PATCH/GET) a configurable JSON or text payload to any HTTP endpoint on IP change."
}

func (e *WebhookExecutor) SecretFields() []string { return nil }

func (e *WebhookExecutor) DefaultConfig() json.RawMessage {
	cfg := webhookConfig{
		Method:       "POST",
		Headers:      map[string]string{"Content-Type": "application/json"},
		BodyTemplate: `{"ip":{{json .NewIP}},"old":{{json .OldIP}},"wan_interface":{{json .WANInterfaceName}},"event":{{json .Event}}}`,
	}
	data, _ := json.Marshal(cfg)
	return data
}

func (e *WebhookExecutor) ConfigSchema() []ConfigField {
	return []ConfigField{
		{Name: "url", Label: "URL", Type: "text", Required: true,
			Placeholder: "https://example.com/webhook",
			HelpText:    "Target endpoint. Supports http:// or https:// and any port."},
		{Name: "method", Label: "HTTP Method", Type: "select", Required: true, Placeholder: "POST",
			HelpText: "POST is the common default. Use PUT/PATCH for REST APIs that expect them; GET has no body."},
		{Name: "headers", Label: "Headers", Type: "textarea", Required: false,
			Placeholder: "{\"Authorization\":\"Bearer ...\",\"X-Custom\":\"value\"}",
			HelpText:    "JSON object of header key-value pairs. Added to every request."},
		{Name: "body_template", Label: "Body Template", Type: "textarea", Required: false,
			Placeholder: "{\"ip\":\"{{.NewIP}}\",\"old\":\"{{.OldIP}}\"}",
			HelpText:    "Go template with {{.NewIP}}, {{.OldIP}}, {{.WANInterfaceName}}, {{.Event}}, {{.Timestamp}}."},
	}
}

// ParseForm converts the generic edit form values for a webhook action
// into a correctly typed config JSON. The crucial detail is Headers:
// the UI submits the textarea contents verbatim, but webhookConfig.Headers
// is map[string]string. buildConfigFromForm would stringify it and every
// Execute would then fail with a type-mismatch unmarshal (#1).
//
// Here we parse the headers textarea as JSON once at save time so the
// stored config is already the right shape.
func (e *WebhookExecutor) ParseForm(form url.Values) (json.RawMessage, error) {
	cfg := webhookConfig{
		URL:          strings.TrimSpace(form.Get("config_url")),
		Method:       strings.ToUpper(strings.TrimSpace(form.Get("config_method"))),
		BodyTemplate: form.Get("config_body_template"),
		Headers:      map[string]string{},
	}
	if cfg.Method == "" {
		cfg.Method = "POST"
	}
	if raw := strings.TrimSpace(form.Get("config_headers")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Headers); err != nil {
			return nil, fmt.Errorf("headers must be a JSON object of string values, e.g. {\"Authorization\":\"Bearer ...\"}: %w", err)
		}
	}
	return json.Marshal(cfg)
}

func (e *WebhookExecutor) ValidateConfig(raw json.RawMessage) error {
	var cfg webhookConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if cfg.URL == "" {
		return fmt.Errorf("url is required")
	}
	if cfg.BodyTemplate != "" {
		if _, err := template.New("test").Funcs(webhookFuncs).Parse(cfg.BodyTemplate); err != nil {
			return fmt.Errorf("invalid body_template: %w", err)
		}
	}
	return nil
}

func (e *WebhookExecutor) Execute(ctx context.Context, event IPEvent, raw json.RawMessage) error {
	var cfg webhookConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	method := strings.ToUpper(cfg.Method)
	if method == "" {
		method = "POST"
	}

	// Render body template (with JSON-safe escaping via {{json .Field}})
	var body string
	if cfg.BodyTemplate != "" {
		tmpl, err := template.New("body").Funcs(webhookFuncs).Parse(cfg.BodyTemplate)
		if err != nil {
			return fmt.Errorf("parsing template: %w", err)
		}
		data := WebhookTemplateData{
			WANInterfaceName: event.WANInterfaceName,
			SourceName:       event.WANInterfaceName, // backward compat alias
			NewIP:            event.NewIP,
			OldIP:            event.OldIP,
			Event:            event.EventType,
			Timestamp:        event.Timestamp,
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return fmt.Errorf("executing template: %w", err)
		}
		body = buf.String()
	}

	// Build request
	var reqBody *bytes.Reader
	if body != "" {
		reqBody = bytes.NewReader([]byte(body))
	}

	var req *http.Request
	var err error
	if reqBody != nil {
		req, err = http.NewRequestWithContext(ctx, method, cfg.URL, reqBody)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, cfg.URL, nil)
	}
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		// Sanitize error to avoid leaking webhook URL in error messages/logs
		return fmt.Errorf("webhook request failed (check URL and connectivity)")
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
