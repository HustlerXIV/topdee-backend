package email

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Mailer sends transactional email via Resend (https://resend.com).
// If APIKey is empty, all sends are silently skipped so the app still
// works locally without email configured.
type Mailer struct {
	APIKey string
	From   string // e.g. "Topdee <noreply@mail.yourdomain.com>"
}

type resendReq struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
}

// Send delivers one transactional email. Non-fatal: logs nothing itself —
// callers should log the returned error if they care.
func (m *Mailer) Send(to, subject, html string) error {
	if m.APIKey == "" {
		return nil // email not configured — skip silently
	}
	body, _ := json.Marshal(resendReq{
		From:    m.From,
		To:      []string{to},
		Subject: subject,
		HTML:    html,
	})
	req, err := http.NewRequest(http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("resend: status %d — %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

// InviteHTML returns the HTML body for a team invite email.
func InviteHTML(workspaceName, inviterEmail, acceptURL string, expiresAt time.Time) string {
	expiry := expiresAt.Format("2 January 2006")
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>You're invited to join %s</title>
</head>
<body style="margin:0;padding:0;background:#f4f4f5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;">
  <table width="100%%" cellpadding="0" cellspacing="0" style="background:#f4f4f5;padding:40px 16px;">
    <tr>
      <td align="center">
        <table width="100%%" cellpadding="0" cellspacing="0" style="max-width:520px;">

          <!-- Logo / brand -->
          <tr>
            <td align="center" style="padding-bottom:24px;">
              <span style="font-size:22px;font-weight:800;color:#18181b;letter-spacing:-0.5px;">Topdee</span>
            </td>
          </tr>

          <!-- Card -->
          <tr>
            <td style="background:#ffffff;border-radius:16px;padding:40px 36px;box-shadow:0 1px 3px rgba(0,0,0,0.07);">

              <p style="margin:0 0 8px;font-size:13px;font-weight:600;color:#71717a;text-transform:uppercase;letter-spacing:0.8px;">Team invitation</p>
              <h1 style="margin:0 0 16px;font-size:24px;font-weight:800;color:#18181b;line-height:1.3;">
                You're invited to join<br/><span style="color:#6366f1;">%s</span>
              </h1>

              <p style="margin:0 0 28px;font-size:15px;color:#52525b;line-height:1.6;">
                <strong>%s</strong> has invited you to collaborate on the <strong>%s</strong> workspace on Topdee — the AI-powered customer messaging platform.
              </p>

              <!-- CTA -->
              <table cellpadding="0" cellspacing="0" style="margin:0 0 28px;">
                <tr>
                  <td style="background:#6366f1;border-radius:10px;">
                    <a href="%s"
                       style="display:inline-block;padding:14px 32px;font-size:15px;font-weight:700;color:#ffffff;text-decoration:none;letter-spacing:0.1px;">
                      Accept invitation →
                    </a>
                  </td>
                </tr>
              </table>

              <p style="margin:0 0 8px;font-size:13px;color:#a1a1aa;">
                Or copy and paste this link into your browser:
              </p>
              <p style="margin:0 0 28px;font-size:12px;color:#6366f1;word-break:break-all;">%s</p>

              <hr style="border:none;border-top:1px solid #f4f4f5;margin:0 0 20px;" />

              <p style="margin:0;font-size:13px;color:#a1a1aa;line-height:1.6;">
                This invitation expires on <strong>%s</strong>. If you weren't expecting this email, you can safely ignore it — no account will be created without your action.
              </p>
            </td>
          </tr>

          <!-- Footer -->
          <tr>
            <td align="center" style="padding-top:24px;">
              <p style="margin:0;font-size:12px;color:#a1a1aa;">
                © %d Topdee · Sent by %s
              </p>
            </td>
          </tr>

        </table>
      </td>
    </tr>
  </table>
</body>
</html>`,
		workspaceName,
		workspaceName,
		inviterEmail, workspaceName,
		acceptURL,
		acceptURL,
		expiry,
		time.Now().Year(), inviterEmail,
	)
}
