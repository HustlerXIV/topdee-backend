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

// ── Notification email templates ─────────────────────────────────────────────

// NewChatHTML returns the HTML body for a "new customer message" notification.
// channel is the provider slug ("line" / "facebook" / etc.).
// appURL is the frontend base URL, e.g. "https://app.example.com".
func NewChatHTML(workspaceName, customerName, preview, channel, appURL string) string {
	channelLabel := channel
	switch channel {
	case "line":
		channelLabel = "LINE"
	case "facebook":
		channelLabel = "Facebook Messenger"
	}
	if preview == "" {
		preview = "(no text)"
	}
	if len(preview) > 200 {
		preview = preview[:200] + "…"
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"/><meta name="viewport" content="width=device-width,initial-scale=1.0"/>
<title>New message from %s</title></head>
<body style="margin:0;padding:0;background:#f4f4f5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;">
<table width="100%%" cellpadding="0" cellspacing="0" style="background:#f4f4f5;padding:40px 16px;">
  <tr><td align="center">
    <table width="100%%" cellpadding="0" cellspacing="0" style="max-width:520px;">
      <tr><td align="center" style="padding-bottom:24px;">
        <span style="font-size:22px;font-weight:800;color:#18181b;letter-spacing:-0.5px;">Topdee</span>
      </td></tr>
      <tr><td style="background:#ffffff;border-radius:16px;padding:40px 36px;box-shadow:0 1px 3px rgba(0,0,0,0.07);">
        <p style="margin:0 0 8px;font-size:13px;font-weight:600;color:#71717a;text-transform:uppercase;letter-spacing:0.8px;">New message · %s</p>
        <h1 style="margin:0 0 16px;font-size:22px;font-weight:800;color:#18181b;">
          💬 %s sent a message
        </h1>
        <div style="background:#f4f4f5;border-radius:10px;padding:16px 20px;margin:0 0 28px;font-size:15px;color:#52525b;font-style:italic;line-height:1.6;">
          "%s"
        </div>
        <table cellpadding="0" cellspacing="0" style="margin:0 0 20px;">
          <tr><td style="background:#6366f1;border-radius:10px;">
            <a href="%s/inbox"
               style="display:inline-block;padding:13px 28px;font-size:14px;font-weight:700;color:#ffffff;text-decoration:none;">
              View in inbox →
            </a>
          </td></tr>
        </table>
        <p style="margin:0;font-size:13px;color:#a1a1aa;">
          Workspace: <strong>%s</strong>
        </p>
      </td></tr>
      <tr><td align="center" style="padding-top:24px;">
        <p style="margin:0;font-size:12px;color:#a1a1aa;">© %d Topdee</p>
      </td></tr>
    </table>
  </td></tr>
</table>
</body></html>`,
		customerName,
		channelLabel,
		customerName,
		preview,
		appURL,
		workspaceName,
		time.Now().Year(),
	)
}

// AICantAnswerHTML returns the HTML body for an "AI couldn't answer" handoff notification.
// appURL is the frontend base URL, e.g. "https://app.example.com".
func AICantAnswerHTML(workspaceName, customerName, preview, channel, appURL string) string {
	channelLabel := channel
	switch channel {
	case "line":
		channelLabel = "LINE"
	case "facebook":
		channelLabel = "Facebook Messenger"
	}
	if preview == "" {
		preview = "(no text)"
	}
	if len(preview) > 200 {
		preview = preview[:200] + "…"
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"/><meta name="viewport" content="width=device-width,initial-scale=1.0"/>
<title>AI needs your help</title></head>
<body style="margin:0;padding:0;background:#f4f4f5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;">
<table width="100%%" cellpadding="0" cellspacing="0" style="background:#f4f4f5;padding:40px 16px;">
  <tr><td align="center">
    <table width="100%%" cellpadding="0" cellspacing="0" style="max-width:520px;">
      <tr><td align="center" style="padding-bottom:24px;">
        <span style="font-size:22px;font-weight:800;color:#18181b;letter-spacing:-0.5px;">Topdee</span>
      </td></tr>
      <tr><td style="background:#ffffff;border-radius:16px;padding:40px 36px;box-shadow:0 1px 3px rgba(0,0,0,0.07);">
        <p style="margin:0 0 8px;font-size:13px;font-weight:600;color:#f97316;text-transform:uppercase;letter-spacing:0.8px;">⚠ Human handoff · %s</p>
        <h1 style="margin:0 0 12px;font-size:22px;font-weight:800;color:#18181b;">
          AI couldn't answer — please step in
        </h1>
        <p style="margin:0 0 16px;font-size:15px;color:#52525b;line-height:1.6;">
          <strong>%s</strong> asked something outside the AI's knowledge. The conversation has been flagged <em>รอทีม</em>.
        </p>
        <div style="background:#fff7ed;border:1px solid #fed7aa;border-radius:10px;padding:16px 20px;margin:0 0 28px;font-size:15px;color:#7c2d12;font-style:italic;line-height:1.6;">
          "%s"
        </div>
        <table cellpadding="0" cellspacing="0" style="margin:0 0 20px;">
          <tr><td style="background:#f97316;border-radius:10px;">
            <a href="%s/inbox"
               style="display:inline-block;padding:13px 28px;font-size:14px;font-weight:700;color:#ffffff;text-decoration:none;">
              Open conversation →
            </a>
          </td></tr>
        </table>
        <p style="margin:0;font-size:13px;color:#a1a1aa;">
          Workspace: <strong>%s</strong>
        </p>
      </td></tr>
      <tr><td align="center" style="padding-top:24px;">
        <p style="margin:0;font-size:12px;color:#a1a1aa;">© %d Topdee</p>
      </td></tr>
    </table>
  </td></tr>
</table>
</body></html>`,
		channelLabel,
		customerName,
		preview,
		appURL,
		workspaceName,
		time.Now().Year(),
	)
}

// QuotaWarningHTML returns the HTML body for an 80% monthly quota warning.
// appURL is the frontend base URL, e.g. "https://app.example.com".
func QuotaWarningHTML(workspaceName string, used, limit int, nextPlan, appURL string) string {
	pct := 0
	if limit > 0 {
		pct = used * 100 / limit
	}
	upgradeRow := ""
	if nextPlan != "" {
		upgradeRow = fmt.Sprintf(`
        <table cellpadding="0" cellspacing="0" style="margin:20px 0 0;">
          <tr><td style="background:#6366f1;border-radius:10px;">
            <a href="%s/billing"
               style="display:inline-block;padding:13px 28px;font-size:14px;font-weight:700;color:#ffffff;text-decoration:none;">
              Upgrade to %s →
            </a>
          </td></tr>
        </table>`, appURL, nextPlan)
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"/><meta name="viewport" content="width=device-width,initial-scale=1.0"/>
<title>Quota warning</title></head>
<body style="margin:0;padding:0;background:#f4f4f5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;">
<table width="100%%" cellpadding="0" cellspacing="0" style="background:#f4f4f5;padding:40px 16px;">
  <tr><td align="center">
    <table width="100%%" cellpadding="0" cellspacing="0" style="max-width:520px;">
      <tr><td align="center" style="padding-bottom:24px;">
        <span style="font-size:22px;font-weight:800;color:#18181b;letter-spacing:-0.5px;">Topdee</span>
      </td></tr>
      <tr><td style="background:#ffffff;border-radius:16px;padding:40px 36px;box-shadow:0 1px 3px rgba(0,0,0,0.07);">
        <p style="margin:0 0 8px;font-size:13px;font-weight:600;color:#eab308;text-transform:uppercase;letter-spacing:0.8px;">⚡ Usage alert</p>
        <h1 style="margin:0 0 12px;font-size:22px;font-weight:800;color:#18181b;">
          You've used %d%% of your monthly quota
        </h1>
        <p style="margin:0 0 20px;font-size:15px;color:#52525b;line-height:1.6;">
          <strong>%s</strong> has used <strong>%d / %d</strong> AI messages this month.
          When the limit is reached, customers will see an upgrade prompt instead of an AI reply.
        </p>
        <!-- Progress bar -->
        <div style="background:#f4f4f5;border-radius:99px;height:10px;overflow:hidden;margin:0 0 28px;">
          <div style="background:#eab308;width:%d%%;height:10px;border-radius:99px;"></div>
        </div>
        %s
        <p style="margin:20px 0 0;font-size:13px;color:#a1a1aa;">
          This warning is sent once per month and will not repeat until next month.
        </p>
      </td></tr>
      <tr><td align="center" style="padding-top:24px;">
        <p style="margin:0;font-size:12px;color:#a1a1aa;">© %d Topdee</p>
      </td></tr>
    </table>
  </td></tr>
</table>
</body></html>`,
		pct,
		workspaceName, used, limit,
		pct,
		upgradeRow,
		time.Now().Year(),
	)
}

// ── Password-reset email template ────────────────────────────────────────────

// ForgotPasswordHTML returns the HTML body for a password-reset request.
// resetURL is the full URL the user clicks to land on the reset-password page,
// e.g. "https://www.top-dee.com/reset-password?token=<raw_token>".
// The link expires in 1 hour — that deadline should be reflected here.
func ForgotPasswordHTML(userName, resetURL string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"/><meta name="viewport" content="width=device-width,initial-scale=1.0"/>
<title>Reset your Topdee password</title></head>
<body style="margin:0;padding:0;background:#f4f4f5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;">
<table width="100%%" cellpadding="0" cellspacing="0" style="background:#f4f4f5;padding:40px 16px;">
  <tr><td align="center">
    <table width="100%%" cellpadding="0" cellspacing="0" style="max-width:520px;">
      <tr><td align="center" style="padding-bottom:24px;">
        <span style="font-size:22px;font-weight:800;color:#18181b;letter-spacing:-0.5px;">Topdee</span>
      </td></tr>
      <tr><td style="background:#ffffff;border-radius:16px;padding:40px 36px;box-shadow:0 1px 3px rgba(0,0,0,0.07);">
        <p style="margin:0 0 8px;font-size:13px;font-weight:600;color:#71717a;text-transform:uppercase;letter-spacing:0.8px;">Password reset</p>
        <h1 style="margin:0 0 16px;font-size:24px;font-weight:800;color:#18181b;line-height:1.3;">
          Reset your password
        </h1>
        <p style="margin:0 0 28px;font-size:15px;color:#52525b;line-height:1.6;">
          Hi <strong>%s</strong>,<br/><br/>
          We received a request to reset the password for your Topdee account.
          Click the button below to choose a new password. This link is valid for <strong>1 hour</strong>.
        </p>
        <table cellpadding="0" cellspacing="0" style="margin:0 0 28px;">
          <tr><td style="background:#6366f1;border-radius:10px;">
            <a href="%s"
               style="display:inline-block;padding:14px 32px;font-size:15px;font-weight:700;color:#ffffff;text-decoration:none;letter-spacing:0.1px;">
              Reset password →
            </a>
          </td></tr>
        </table>
        <p style="margin:0 0 8px;font-size:13px;color:#a1a1aa;">
          Or copy and paste this link into your browser:
        </p>
        <p style="margin:0 0 28px;font-size:12px;color:#6366f1;word-break:break-all;">%s</p>
        <hr style="border:none;border-top:1px solid #f4f4f5;margin:0 0 20px;"/>
        <p style="margin:0;font-size:13px;color:#a1a1aa;line-height:1.6;">
          If you didn't request a password reset, you can safely ignore this email — your password will not change.
          This link expires in 1 hour.
        </p>
      </td></tr>
      <tr><td align="center" style="padding-top:24px;">
        <p style="margin:0;font-size:12px;color:#a1a1aa;">© %d Topdee</p>
      </td></tr>
    </table>
  </td></tr>
</table>
</body></html>`,
		userName,
		resetURL,
		resetURL,
		time.Now().Year(),
	)
}

// ── Invite email template ─────────────────────────────────────────────────────

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
