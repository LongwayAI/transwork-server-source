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
	registerDesktopSubscriptionRoutes(apiRouter)
	registerAdminSubscriptionRoutes(apiRouter)
	registerAdminSubscriptionSummaryRoute(apiRouter)
	registerWaitlistRoutes(apiRouter)
}

// RegisterPublicRoutes registers root-level public (no-auth) Gressio routes that
// must live outside the /api namespace. The top-up result page is where Stripe
// Checkout redirects the browser after a payment, so it has to be reachable
// without a token and at a clean, first-party path.
func RegisterPublicRoutes(router *gin.Engine) {
	router.GET("/topup/result", twhandler.TopupResult)
	// Parallel Stripe endpoint for the recurring-subscription lifecycle. Root
	// engine, no auth middleware, first-party path — mirrors /topup/result. Stripe
	// signs each request; the handler verifies with the overlay signing secret.
	router.POST("/transwork/stripe/subscription-webhook", twhandler.StripeSubscriptionWebhook)
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
		authRoute.POST("/redeem", middleware.CriticalRateLimit(), twhandler.AuthRedeem)
		authRoute.POST("/claim-free-credit", twhandler.ClaimFreeCredit)
	}
}

// registerWaitlistRoutes exposes the desktop "join the waitlist" form endpoint,
// which emails the request to the Gressio inbox. TokenAuth() because the invite
// gate that hosts the form is only shown to authenticated (0-quota) users, which
// also keeps the endpoint from being an anonymous spam relay.
func registerWaitlistRoutes(apiRouter *gin.RouterGroup) {
	waitlistRoute := apiRouter.Group("/waitlist")
	waitlistRoute.Use(middleware.TokenAuth())
	{
		waitlistRoute.POST("", twhandler.SubmitWaitlist)
	}
	// Admin listing for the dashboard (session-cookie admin auth, like the other
	// /api/transwork/*/admin routes), so the waitlist is browsable server-side.
	adminRoute := apiRouter.Group("/transwork/waitlist/admin")
	adminRoute.Use(middleware.AdminAuth())
	{
		adminRoute.GET("", twhandler.ListWaitlist)
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
		topupRoute.GET("/tiers", GetRechargeTiers)
		topupRoute.POST("/amount", controller.RequestAmount)
		topupRoute.POST("/pay", middleware.CriticalRateLimit(), controller.RequestEpay)
		topupRoute.POST("/stripe/pay", middleware.CriticalRateLimit(), controller.RequestStripePay)
		topupRoute.POST("/creem/pay", middleware.CriticalRateLimit(), controller.RequestCreemPay)
	}
}

// registerDesktopSubscriptionRoutes exposes the recurring-subscription status
// and Stripe Customer Portal endpoints to the desktop client, authed with the
// same long-lived bearer token via TokenAuth() as the top-up routes (the web
// dashboard's session-cookie UserAuth requires a New-Api-User header the desktop
// token cannot supply — design B6). Portal creation calls Stripe's API, so it is
// additionally gated with CriticalRateLimit(), mirroring the stripe/pay routes.
func registerDesktopSubscriptionRoutes(apiRouter *gin.RouterGroup) {
	subRoute := apiRouter.Group("/desktop/subscription")
	subRoute.Use(middleware.TokenAuth())
	{
		subRoute.GET("/self", twhandler.GetDesktopSubscriptionStatus)
		subRoute.GET("/plans", twhandler.ListDesktopSubscriptionPlans)
		subRoute.POST("/portal", middleware.CriticalRateLimit(), twhandler.CreateSubscriptionPortalSession)
		subRoute.POST("/pay", middleware.CriticalRateLimit(), twhandler.CreateDesktopSubscriptionCheckout)
	}
}

// registerAdminSubscriptionRoutes exposes the overlay admin actions for the
// recurring-subscription lifecycle: propagating a local lapse to Stripe (B7) and
// listing the overlay link rows the per-user modal merges by user_subscription_id
// (F2). Both are guarded by AdminAuth(), the same middleware upstream uses for
// /api/subscription/admin/*.
func registerAdminSubscriptionRoutes(apiRouter *gin.RouterGroup) {
	adminRoute := apiRouter.Group("/transwork/subscription/admin")
	adminRoute.Use(middleware.AdminAuth())
	{
		adminRoute.POST("/cancel-stripe", twhandler.CancelStripeSubscription)
		adminRoute.GET("/links", twhandler.ListStripeSubscriptionLinks)
	}
}

// registerAdminSubscriptionSummaryRoute exposes a batch lookup of each user's
// primary in-period subscription for the admin User Management table, so the
// dashboard can render a subscription column without an N+1 fetch per row.
// Static /users/summary sits beside upstream's param route
// /users/:id/subscriptions; modern Gin matches the static segment first.
func registerAdminSubscriptionSummaryRoute(apiRouter *gin.RouterGroup) {
	adminRoute := apiRouter.Group("/subscription/admin")
	adminRoute.Use(middleware.AdminAuth())
	{
		adminRoute.POST("/users/summary", twhandler.AdminSubscriptionUsersSummary)
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
