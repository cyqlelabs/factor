package phone

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// twilioClient is the whole carrier dependency: one form-encoded POST for SMS.
// Voice calls go through the voice shell, which owns the media stream.
type twilioClient struct {
	base   string
	sid    string
	token  string
	client *http.Client
}

func newTwilioClient(cfg Config) *twilioClient {
	base := cfg.APIBase
	if base == "" {
		base = "https://api.twilio.com"
	}
	return &twilioClient{
		base:   base,
		sid:    cfg.TwilioAccountSID,
		token:  cfg.TwilioAuthToken,
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

// redact keeps the auth token out of error chains: transport errors can embed
// request URLs and credentials, and errors end up in logs and tool results.
func (t *twilioClient) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if t.token != "" {
		msg = strings.ReplaceAll(msg, t.token, "[token]")
	}
	return fmt.Errorf("%s", msg)
}

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
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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
