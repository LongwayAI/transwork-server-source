package transwork

import (
	"net/http"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	twhandler "github.com/QuantumNous/new-api/transwork/handler"

	"github.com/gin-gonic/gin"
)

// RegisterAPIRoutes isolates Gressio-specific product routes from the
// upstream-heavy API router.
func RegisterAPIRoutes(apiRouter *gin.RouterGroup) {
	registerElevenLabsRoutes(apiRouter)
	registerAudioRoutes(apiRouter)
	registerDesktopLoginRoutes(apiRouter)
	registerAuthRoutes(apiRouter)
	registerTopupRoutes(apiRouter)
}

// RegisterPublicRoutes registers root-level public (no-auth) Gressio routes that
// must live outside the /api namespace. The top-up result page is where Stripe
// Checkout redirects the browser after a payment, so it has to be reachable
// without a token and at a clean, first-party path.
func RegisterPublicRoutes(router *gin.Engine) {
	router.GET("/topup/result", twhandler.TopupResult)
}

func RegisterChannelRoutes(channelRoute *gin.RouterGroup) {
	channelRoute.POST("/codex/oauth/start", controller.StartCodexOAuth)
	channelRoute.POST("/codex/oauth/complete", controller.CompleteCodexOAuth)
	channelRoute.POST("/:id/codex/oauth/start", controller.StartCodexOAuthForChannel)
	channelRoute.POST("/:id/codex/oauth/complete", controller.CompleteCodexOAuthForChannel)
	channelRoute.POST("/:id/codex/refresh", controller.RefreshCodexChannelCredential)
	channelRoute.GET("/:id/codex/usage", controller.GetCodexChannelUsage)
}

func registerElevenLabsRoutes(apiRouter *gin.RouterGroup) {
	elevenlabsRoute := apiRouter.Group("/elevenlabs")
	{
		elevenlabsRoute.POST("/token", middleware.TokenAuth(), twhandler.CreateElevenLabsTempToken)
		elevenlabsRoute.POST("/usage", middleware.TokenAuth(), twhandler.ReportElevenLabsUsage)
		elevenlabsRoute.GET("/status", twhandler.GetElevenLabsTokenStatus)
	}
}

func registerAudioRoutes(apiRouter *gin.RouterGroup) {
	audioRoute := apiRouter.Group("")
	audioRoute.Use(middleware.TokenAuth())
	{
		audioRoute.POST("/upload-url", twhandler.GenerateAudioUploadURL)
		audioRoute.POST("/transcribe", twhandler.TranscribeAudio)
	}
}

func registerDesktopLoginRoutes(apiRouter *gin.RouterGroup) {
	group := apiRouter.Group("/desktop-login")
	{
		group.POST("/start", twhandler.StartDesktopLogin)
		group.GET("/callback", twhandler.CompleteDesktopLogin)
		group.POST("/exchange", twhandler.ExchangeDesktopLoginBootstrap)
	}
}

func registerAuthRoutes(apiRouter *gin.RouterGroup) {
	authRoute := apiRouter.Group("/auth")
	authRoute.Use(middleware.TokenAuth())
	{
		authRoute.GET("/check", twhandler.AuthCheck)
		authRoute.POST("/logout", twhandler.AuthLogout)
		authRoute.POST("/redeem", twhandler.AuthRedeem)
	}
}

// registerTopupRoutes exposes the existing upstream top-up controllers to the
// desktop client, which authenticates with a long-lived bearer token via
// TokenAuth() (the web dashboard uses session-cookie UserAuth, which the
// desktop token does not satisfy). The controllers only read c.GetInt("id"),
// which TokenAuth sets, so they work unchanged.
func registerTopupRoutes(apiRouter *gin.RouterGroup) {
	topupRoute := apiRouter.Group("/desktop/topup")
	topupRoute.Use(middleware.TokenAuth())
	topupRoute.Use(rechargeGate())
	{
		topupRoute.GET("/info", controller.GetTopUpInfo)
		topupRoute.POST("/amount", controller.RequestAmount)
		topupRoute.POST("/pay", middleware.CriticalRateLimit(), controller.RequestEpay)
		topupRoute.POST("/stripe/pay", middleware.CriticalRateLimit(), controller.RequestStripePay)
		topupRoute.POST("/creem/pay", middleware.CriticalRateLimit(), controller.RequestCreemPay)
	}
}

// rechargeGate blocks desktop top-up requests with 403 Forbidden when recharge
// is disabled by design (master switch off or no payment provider configured),
// enforcing server-side the same gate the client UI honors via enableRecharge —
// so a scripted client with a valid token cannot bypass it.
func rechargeGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !twhandler.RechargeEnabled() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"message": "error", "data": "Recharge is disabled"})
			return
		}
	}
}
