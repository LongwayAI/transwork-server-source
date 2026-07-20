package handler

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/model"
	twmodel "github.com/QuantumNous/new-api/transwork/model"
	"github.com/gin-gonic/gin"
)

type waitlistRequest struct {
	Name    string `json:"name"`
	Job     string `json:"job"`
	Role    string `json:"role"`
	UseCase string `json:"useCase"`
}

// SubmitWaitlist stores a desktop "join the waitlist" submission in the overlay
// waitlist table. The applicant's email is taken from the authenticated account
// (verified at login), NOT the form, so it is always a reachable address and
// cannot be spoofed. Must sit behind middleware.TokenAuth().
func SubmitWaitlist(c *gin.Context) {
	userID := c.GetInt("id")
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user in context"})
		return
	}

	var req waitlistRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Job = strings.TrimSpace(req.Job)
	req.Role = strings.TrimSpace(req.Role)
	req.UseCase = strings.TrimSpace(req.UseCase)
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	email := ""
	if user, err := model.GetUserById(userID, false); err == nil && user.Id != 0 {
		email = user.Email
	}

	entry := twmodel.WaitlistEntry{
		UserId:  userID,
		Email:   email,
		Name:    req.Name,
		Job:     req.Job,
		Role:    req.Role,
		UseCase: req.UseCase,
	}
	if err := model.DB.Create(&entry).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ListWaitlist returns waitlist submissions, newest first, for the admin
// dashboard. Must sit behind middleware.AdminAuth().
func ListWaitlist(c *gin.Context) {
	var entries []twmodel.WaitlistEntry
	if err := model.DB.Order("id desc").Limit(500).Find(&entries).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error(), "success": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": entries})
}
