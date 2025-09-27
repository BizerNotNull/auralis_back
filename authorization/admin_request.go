package authorization

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type adminRequestPayload struct {
	Source   string `json:"source"`
	Message  string `json:"message"`
	UserID   uint   `json:"user_id"`
	Username string `json:"username"`
}

type adminRequestMailer struct {
	host      string
	port      int
	username  string
	password  string
	from      string
	recipient string
	subject   string
}

func newAdminRequestMailerFromEnv() (*adminRequestMailer, error) {
	recipient := sanitizeMailHeader(os.Getenv("ADMIN_REQUEST_RECIPIENT_EMAIL"))
	if recipient == "" {
		return nil, errors.New("admin request recipient email is not configured")
	}

	host := strings.TrimSpace(os.Getenv("ADMIN_REQUEST_SMTP_HOST"))
	if host == "" {
		return nil, errors.New("admin request SMTP host is not configured")
	}

	portValue := strings.TrimSpace(os.Getenv("ADMIN_REQUEST_SMTP_PORT"))
	if portValue == "" {
		portValue = "587"
	}
	port, err := strconv.Atoi(portValue)
	if err != nil || port <= 0 {
		return nil, fmt.Errorf("admin request SMTP port is invalid: %s", portValue)
	}

	username := strings.TrimSpace(os.Getenv("ADMIN_REQUEST_SMTP_USERNAME"))
	password := os.Getenv("ADMIN_REQUEST_SMTP_PASSWORD")
	mailFrom := sanitizeMailHeader(os.Getenv("ADMIN_REQUEST_MAIL_FROM"))
	if mailFrom == "" {
		mailFrom = username
	}

	if username == "" || strings.TrimSpace(password) == "" {
		return nil, errors.New("admin request SMTP credentials are not configured")
	}
	if mailFrom == "" {
		return nil, errors.New("admin request mail sender address is not configured")
	}

	subject := sanitizeMailHeader(os.Getenv("ADMIN_REQUEST_MAIL_SUBJECT"))

	return &adminRequestMailer{
		host:      host,
		port:      port,
		username:  username,
		password:  password,
		from:      mailFrom,
		recipient: recipient,
		subject:   subject,
	}, nil
}

func (m *adminRequestMailer) Send(user *User, payload *adminRequestPayload) error {
	if m == nil {
		return errors.New("admin request mailer not configured")
	}
	if user == nil {
		return errors.New("user information is required")
	}

	subject := m.subject
	if subject == "" {
		subject = "Admin Access Request"
	}
	subject = encodeMailSubject(subject)

	now := time.Now().UTC()

	var bodyBuilder strings.Builder
	bodyBuilder.WriteString("A new administrator access request has been submitted.\r\n\r\n")
	bodyBuilder.WriteString(fmt.Sprintf("User ID: %d\r\n", user.ID))
	if user.Username != "" {
		bodyBuilder.WriteString(fmt.Sprintf("Username: %s\r\n", sanitizeMailHeader(user.Username)))
	}
	if user.DisplayName != "" {
		bodyBuilder.WriteString(fmt.Sprintf("Display Name: %s\r\n", sanitizeMailHeader(user.DisplayName)))
	}
	if user.Nickname != "" {
		bodyBuilder.WriteString(fmt.Sprintf("Nickname: %s\r\n", sanitizeMailHeader(user.Nickname)))
	}
	if user.Email != "" {
		bodyBuilder.WriteString(fmt.Sprintf("Email: %s\r\n", sanitizeMailHeader(user.Email)))
	}
	bodyBuilder.WriteString(fmt.Sprintf("Requested At (UTC): %s\r\n", now.Format(time.RFC3339)))

	if payload != nil {
		if payload.UserID != 0 {
			bodyBuilder.WriteString(fmt.Sprintf("Client Reported User ID: %d\r\n", payload.UserID))
		}
		if payload.Username != "" {
			bodyBuilder.WriteString(fmt.Sprintf("Client Reported Username: %s\r\n", sanitizeMailHeader(payload.Username)))
		}
		if payload.Source != "" {
			bodyBuilder.WriteString(fmt.Sprintf("Source: %s\r\n", sanitizeMailHeader(payload.Source)))
		}
		if strings.TrimSpace(payload.Message) != "" {
			bodyBuilder.WriteString("\r\nAdditional Message:\r\n")
			bodyBuilder.WriteString(strings.TrimSpace(payload.Message))
			bodyBuilder.WriteString("\r\n")
		}
	}

	headers := []string{
		fmt.Sprintf("From: %s", m.from),
		fmt.Sprintf("To: %s", m.recipient),
		fmt.Sprintf("Subject: %s", subject),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
		fmt.Sprintf("Date: %s", now.Format(time.RFC1123Z)),
	}

	var messageBuilder strings.Builder
	for _, header := range headers {
		messageBuilder.WriteString(header)
		messageBuilder.WriteString("\r\n")
	}
	messageBuilder.WriteString("\r\n")
	messageBuilder.WriteString(bodyBuilder.String())

	address := fmt.Sprintf("%s:%d", m.host, m.port)
	auth := smtp.PlainAuth("", m.username, m.password, m.host)

	return smtp.SendMail(address, auth, m.from, []string{m.recipient}, []byte(messageBuilder.String()))
}

func encodeMailSubject(subject string) string {
	if subject == "" {
		return subject
	}
	if isASCII(subject) {
		return subject
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(subject))
	return fmt.Sprintf("=?UTF-8?B?%s?=", encoded)
}

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

func sanitizeMailHeader(value string) string {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.ReplaceAll(trimmed, "\r", " ")
	trimmed = strings.ReplaceAll(trimmed, "\n", " ")
	return trimmed
}

func (m *Module) handleAdminRequest(c *gin.Context) {
	if m == nil || m.userStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "admin request service unavailable"})
		return
	}
	if m.adminRequestMailer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "admin request mailer is not configured"})
		return
	}

	var payload adminRequestPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}

	claims := jwt.ExtractClaims(c)
	userID := extractUserID(claims)
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return
	}

	ctx := c.Request.Context()
	user, err := m.userStore.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
		}
		return
	}

	if err := m.adminRequestMailer.Send(user, &payload); err != nil {
		log.Printf("authorization: failed to send admin request email: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to notify administrator"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "admin request submitted"})
}
