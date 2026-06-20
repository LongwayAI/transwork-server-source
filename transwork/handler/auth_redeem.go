package handler

import (
	"net/http"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

type redeemRequest struct {
	Code string `json:"code" binding:"required"`
}

// AuthRedeem redeems an invite/redemption code for the authenticated user and
// credits the associated quota. Must sit behind middleware.TokenAuth().
func AuthRedeem(c *gin.Context) {
	userID := c.GetInt("id")
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user in context"})
		return
	}

	var req redeemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code is required"})
		return
	}

	quotaAdded, err := model.Redeem(req.Code, userID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"quotaAdded": quotaAdded})
}
