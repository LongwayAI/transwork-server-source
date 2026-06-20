package handler

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// AuthLogout deletes the Token row corresponding to the bearer key on the
// current request and returns an OIDC end-session URL so the desktop client
// can also kill the IdP browser session. Must sit behind middleware.TokenAuth().
//
// The endSessionUrl field is empty when we have no id_token on hand
// (e.g. server restarted since sign-in) or no end-session endpoint can be
// derived from the configured AuthorizationEndpoint; in either case the local
// token is still deleted and the client should fall through to its keyring
// cleanup.
func AuthLogout(c *gin.Context) {
	userID := c.GetInt("id")
	tokenID := c.GetInt("token_id")
	if userID == 0 || tokenID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no token in context"})
		return
	}
	var endSessionURL string
	if v, ok := idTokenByTokenID.LoadAndDelete(tokenID); ok {
		if idToken, _ := v.(string); idToken != "" {
			endSessionURL = buildDesktopEndSessionURL(idToken)
		}
	}
	if err := model.DeleteTokenById(tokenID, userID); err != nil {
		common.SysLog("auth logout failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "endSessionUrl": endSessionURL})
}
