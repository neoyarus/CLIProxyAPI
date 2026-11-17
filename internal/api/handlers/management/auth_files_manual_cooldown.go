package management

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// SetCredentialCooldown pauses a credential until a given time, or resumes it.
//
//	POST /v8/management/credentials/cooldown
//	{"name": "<file name>", "auth_index": "<optional>", "until": "<RFC 3339>"}
//	{"name": "<file name>", "duration_seconds": 3600}
//	{"name": "<file name>"}                       resumes (also "until": "" or "duration_seconds": 0)
//
// While paused the credential is treated as cooling down: routing skips it, sessions
// bound to it move to another credential, and it returns by itself when the pause ends.
// The pause is stored in the credential's auth file, so it survives restarts.
func (h *Handler) SetCredentialCooldown(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var req struct {
		Name            string `json:"name"`
		AuthIndex       string `json:"auth_index"`
		Until           string `json:"until"`
		DurationSeconds int64  `json:"duration_seconds"`
	}
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(clienterror.HTTPStatusFromErrorOr(errBind, http.StatusBadRequest), gin.H{"error": "invalid request body"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	now := time.Now()
	var until time.Time
	switch {
	case strings.TrimSpace(req.Until) != "" && req.DurationSeconds != 0:
		c.JSON(http.StatusBadRequest, gin.H{"error": "use until or duration_seconds, not both"})
		return
	case strings.TrimSpace(req.Until) != "":
		parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(req.Until))
		if errParse != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be an RFC 3339 time"})
			return
		}
		until = parsed
	case req.DurationSeconds < 0:
		c.JSON(http.StatusBadRequest, gin.H{"error": "duration_seconds must not be negative"})
		return
	case req.DurationSeconds > 0:
		until = now.Add(time.Duration(req.DurationSeconds) * time.Second)
	}
	if !until.IsZero() {
		if !until.After(now) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "the cooldown must end in the future"})
			return
		}
		if until.Sub(now) > coreauth.MaxManualCooldown {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("the cooldown may last at most %d days", int(coreauth.MaxManualCooldown.Hours()/24))})
			return
		}
	}

	h.authStatusMu.Lock()
	defer h.authStatusMu.Unlock()
	ctx := c.Request.Context()
	targetAuth, _ := h.lookupAuthFile(name, req.AuthIndex)
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if coreauth.IsPluginVirtualAuth(targetAuth) || coreauth.IsConfigAPIKeyAuth(targetAuth) {
		c.JSON(http.StatusConflict, gin.H{"error": "this credential cannot be paused; disable it instead"})
		return
	}
	updatedAuth, committed, errModify := h.authManager.Modify(ctx, targetAuth.ID, targetAuth.RegistrationEpoch, func(current *coreauth.Auth) error {
		coreauth.SetManualCooldown(current, until)
		return nil
	})
	if errModify != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", errModify)})
		return
	}
	if !committed || updatedAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if errHook := h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{updatedAuth}); errHook != nil {
		log.Errorf("post-auth persist hook failed for cooldown update on %s: %v", targetAuth.ID, errHook)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to synchronize auth runtime: %v", errHook)})
		return
	}
	response := gin.H{"status": "ok", "cooldown_until": ""}
	if !until.IsZero() {
		response["cooldown_until"] = until.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, response)
}
