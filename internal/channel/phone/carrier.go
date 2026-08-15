package phone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// telnyxAPIBase is the carrier's REST root, shared by the SMS client here and
// the wizard's credential probe.
const telnyxAPIBase = "https://api.telnyx.com"

// carrierClient is the whole carrier dependency in Go: one POST to send a text.
// Voice calls go through the voice shell, which owns the media stream.
type carrierClient interface {
	sendSMS(ctx context.Context, from, to, body string) (string, error)
}

// newCarrierClient builds the client for the configured carrier. The config is
// already validated, so an unknown name cannot reach here.
func newCarrierClient(cfg Config) carrierClient {
	if cfg.Carrier == carrierTelnyx {
		return newTelnyxClient(cfg)
	}
	return newTwilioClient(cfg)
}

// carrierBase is where the carrier's REST API lives, unless a test pointed the
// section somewhere else.
func carrierBase(cfg Config, fallback string) string {
	if cfg.APIBase != "" {
		return cfg.APIBase
	}
	return fallback
}

// redactSecret keeps a credential out of error chains: transport errors can
// embed request URLs and credentials, and errors end up in logs and tool
// results.
func redactSecret(err error, secret string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, "[token]")
	}
	return fmt.Errorf("%s", msg)
}

// readBody reads a carrier's answer, capped: a carrier that starts streaming
// megabytes at us is a bug, not a message id.
func readBody(resp *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ---- twilio ----------------------------------------------------------------

type twilioClient struct {
	base   string
	sid    string
	token  string
	client *http.Client
}

func newTwilioClient(cfg Config) *twilioClient {
	return &twilioClient{
		base:   carrierBase(cfg, "https://api.twilio.com"),
		sid:    cfg.TwilioAccountSID,
		token:  cfg.TwilioAuthToken,
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

func (t *twilioClient) redact(err error) error { return redactSecret(err, t.token) }

// sendSMS posts one message and returns the carrier's message SID.
func (t *twilioClient) sendSMS(ctx context.Context, from, to, body string) (string, error) {
	form := url.Values{"To": {to}, "From": {from}, "Body": {body}}
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json", t.base, url.PathEscape(t.sid))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", t.redact(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(t.sid, t.token)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", t.redact(err)
	}
	defer resp.Body.Close()
	data, err := readBody(resp)
	if err != nil {
		return "", t.redact(err)
	}
	var parsed struct {
		SID     string `json:"sid"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	_ = json.Unmarshal(data, &parsed)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := parsed.Message
		if detail == "" {
			detail = strings.TrimSpace(string(data))
		}
		return "", t.redact(fmt.Errorf("twilio rejected the message: HTTP %d %s", resp.StatusCode, detail))
	}
	if parsed.SID == "" {
		return "", fmt.Errorf("twilio accepted the message but returned no sid")
	}
	return parsed.SID, nil
}

// ---- telnyx ----------------------------------------------------------------

type telnyxClient struct {
	base   string
	key    string
	client *http.Client
}

func newTelnyxClient(cfg Config) *telnyxClient {
	return &telnyxClient{
		base:   carrierBase(cfg, telnyxAPIBase),
		key:    cfg.TelnyxAPIKey,
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

func (t *telnyxClient) redact(err error) error { return redactSecret(err, t.key) }

// sendSMS posts one message and returns the carrier's message id. Telnyx picks
// the messaging profile from the sending number, so there is nothing to name
// here beyond the two numbers and the text.
func (t *telnyxClient) sendSMS(ctx context.Context, from, to, body string) (string, error) {
	payload, err := json.Marshal(map[string]string{"from": from, "to": to, "text": body})
	if err != nil {
		return "", t.redact(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/v2/messages", bytes.NewReader(payload))
	if err != nil {
		return "", t.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.key)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", t.redact(err)
	}
	defer resp.Body.Close()
	data, err := readBody(resp)
	if err != nil {
		return "", t.redact(err)
	}
	var parsed struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		Errors []telnyxError `json:"errors"`
	}
	_ = json.Unmarshal(data, &parsed)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", t.redact(fmt.Errorf("telnyx rejected the message: HTTP %d %s",
			resp.StatusCode, telnyxDetail(parsed.Errors, data)))
	}
	if parsed.Data.ID == "" {
		return "", fmt.Errorf("telnyx accepted the message but returned no id")
	}
	return parsed.Data.ID, nil
}

// telnyxError is how Telnyx reports every failure: an array of them, whatever
// the endpoint.
type telnyxError struct {
	Detail string `json:"detail"`
	Title  string `json:"title"`
}

// telnyxDetail renders the carrier's error array, falling back to the raw body
// so an unexpected shape still says something.
func telnyxDetail(errs []telnyxError, raw []byte) string {
	for _, e := range errs {
		if detail := strings.TrimSpace(e.Detail); detail != "" {
			return detail
		}
		if title := strings.TrimSpace(e.Title); title != "" {
			return title
		}
	}
	return strings.TrimSpace(string(raw))
}
