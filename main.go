package main

import (
	"bytes"
	"embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	_ "github.com/QuantumNous/new-api/setting/performance_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/transwork"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	_ "net/http/pprof"
)

//go:embed web/default/dist
var buildFS embed.FS

//go:embed web/default/dist/index.html
var indexPage []byte

//go:embed web/classic/dist
var classicBuildFS embed.FS

//go:embed web/classic/dist/index.html
var classicIndexPage []byte

func main() {
	startTime := time.Now()

	err := InitResources()
	if err != nil {
		common.FatalLog("failed to initialize resources: " + err.Error())
		return
	}

	common.SysLog("New API " + common.Version + " started")
	if os.Getenv("GIN_MODE") != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}
	if common.DebugEnabled {
		common.SysLog("running in debug mode")
	}

	defer func() {
		err := model.CloseDB()
		if err != nil {
			common.FatalLog("failed to close database: " + err.Error())
		}
	}()

	if common.RedisEnabled {
		// for compatibility with old versions
		common.MemoryCacheEnabled = true
	}
	if common.MemoryCacheEnabled {
		common.SysLog("memory cache enabled")
		common.SysLog(fmt.Sprintf("sync frequency: %d seconds", common.SyncFrequency))

		// Add panic recovery and retry for InitChannelCache
		func() {
			defer func() {
				if r := recover(); r != nil {
					common.SysLog(fmt.Sprintf("InitChannelCache panic: %v, retrying once", r))
					// Retry once
					_, _, fixErr := model.FixAbility()
					if fixErr != nil {
						common.FatalLog(fmt.Sprintf("InitChannelCache failed: %s", fixErr.Error()))
					}
				}
			}()
			model.InitChannelCache()
		}()

		go model.SyncChannelCache(common.SyncFrequency)
	}

	// 热更新配置
	go model.SyncOptions(common.SyncFrequency)

	// 数据看板
	go model.UpdateQuotaData()

	if os.Getenv("CHANNEL_UPDATE_FREQUENCY") != "" {
		frequency, err := strconv.Atoi(os.Getenv("CHANNEL_UPDATE_FREQUENCY"))
		if err != nil {
			common.FatalLog("failed to parse CHANNEL_UPDATE_FREQUENCY: " + err.Error())
		}
		go controller.AutomaticallyUpdateChannels(frequency)
	}

	go controller.AutomaticallyTestChannels()

	// Codex credential auto-refresh check every 10 minutes, refresh when expires within 1 day
	service.StartCodexCredentialAutoRefreshTask()

	// Subscription quota reset task (daily/weekly/monthly/custom)
	service.StartSubscriptionQuotaResetTask()

	// Wire task polling adaptor factory (breaks service -> relay import cycle)
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
		a := relay.GetTaskAdaptor(platform)
		if a == nil {
			return nil
		}
		return a
	}

	// Channel upstream model update check task
	controller.StartChannelUpstreamModelUpdateTask()

	if common.IsMasterNode && constant.UpdateTask {
		gopool.Go(func() {
			controller.UpdateMidjourneyTaskBulk()
		})
		gopool.Go(func() {
			controller.UpdateTaskBulk()
		})
	}
	if os.Getenv("BATCH_UPDATE_ENABLED") == "true" {
		common.BatchUpdateEnabled = true
		common.SysLog("batch update enabled with interval " + strconv.Itoa(common.BatchUpdateInterval) + "s")
		model.InitBatchUpdater()
	}

	if os.Getenv("ENABLE_PPROF") == "true" {
		gopool.Go(func() {
			log.Println(http.ListenAndServe("0.0.0.0:8005", nil))
		})
		go common.Monitor()
		common.SysLog("pprof enabled")
	}

	err = common.StartPyroScope()
	if err != nil {
		common.SysError(fmt.Sprintf("start pyroscope error : %v", err))
	}

	// Initialize HTTP server
	server := gin.New()
	// Gin trusts every peer by default (trusted CIDRs 0.0.0.0/0 + ::/0), so c.ClientIP()
	// returns a caller-supplied X-Forwarded-For and every IP-keyed control — the rate
	// limiters, the token IP allowlist, Turnstile's remoteip — is bypassable with one
	// header. Trust only the reverse proxy that actually fronts us.
	//
	// TRUSTED_PROXIES takes a comma-separated CIDR list. It defaults to loopback only, so
	// a deployment whose proxy arrives over a container bridge must name that CIDR (the
	// transwork compose overlay does). Set it to an empty string to trust nothing and key
	// off RemoteAddr instead (correct only when exposed directly, with no proxy).
	if err := setTrustedProxies(server); err != nil {
		common.FatalLog("failed to set trusted proxies: " + err.Error())
	}
	server.Use(gin.CustomRecovery(func(c *gin.Context, err any) {
		common.SysLog(fmt.Sprintf("panic detected: %v", err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("Panic detected, error: %v. Please submit a issue here: https://github.com/Calcium-Ion/new-api", err),
				"type":    "new_api_panic",
			},
		})
	}))
	// This will cause SSE not to work!!!
	//server.Use(gzip.Gzip(gzip.DefaultCompression))
	server.Use(middleware.RequestId())
	server.Use(middleware.PoweredBy())
	server.Use(middleware.I18n())
	middleware.SetUpLogger(server)
	// Initialize session store
	secureCookie, err := sessionCookieSecure()
	if err != nil {
		common.FatalLog("failed to configure session cookie: " + err.Error())
	}
	store := cookie.NewStore([]byte(common.SessionSecret))
	store.Options(sessions.Options{
		Path:     "/",
		MaxAge:   2592000, // 30 days
		HttpOnly: true,
		Secure:   secureCookie,
		SameSite: http.SameSiteStrictMode,
	})
	server.Use(sessions.Sessions("session", store))

	InjectUmamiAnalytics()
	InjectGoogleAnalytics()

	// 设置路由
	router.SetRouter(server, router.ThemeAssets{
		DefaultBuildFS:   buildFS,
		DefaultIndexPage: indexPage,
		ClassicBuildFS:   classicBuildFS,
		ClassicIndexPage: classicIndexPage,
	})
	var port = os.Getenv("PORT")
	if port == "" {
		port = strconv.Itoa(*common.Port)
	}

	// Log startup success message
	common.LogStartupSuccess(startTime, port)

	// Not server.Run: gin's Run has no signal handling, so SIGTERM from a deploy
	// kills the process mid-response and drops buffered billing counters.
	err = transwork.ListenAndServeGracefully(server, port)
	if err != nil {
		common.FatalLog("failed to start HTTP server: " + err.Error())
	}
}

// defaultTrustedProxies is deliberately loopback-only: it fails closed. Any deployment
// where the proxy reaches us over a container bridge or another private hop must name
// that CIDR in TRUSTED_PROXIES (the transwork compose overlay does).
//
// Trusting RFC1918 wholesale here would be fail-open: the base compose publishes on
// 0.0.0.0, and GCP's default-allow-internal rule permits the whole 10.128.0.0/9 to every
// port, so any other host in the VPC would become a trusted proxy and could resume
// spoofing X-Forwarded-For. Getting this wrong is silent; the loopback-only default
// instead fails loudly (every client collapses into one rate-limit bucket) if the
// deployment forgets to set TRUSTED_PROXIES.
const defaultTrustedProxies = "127.0.0.1/32,::1/128"

func setTrustedProxies(server *gin.Engine) error {
	// os.LookupEnv rather than common.GetEnvOrDefaultString: the latter cannot tell an
	// unset variable from one set to "", and "" is meaningful here (trust nothing).
	raw, ok := os.LookupEnv("TRUSTED_PROXIES")
	if !ok {
		raw = defaultTrustedProxies
	}
	if raw = strings.TrimSpace(raw); raw == "" {
		// Trust nothing: ClientIP() falls back to RemoteAddr, X-Forwarded-For is ignored.
		return server.SetTrustedProxies(nil)
	}
	proxies := make([]string, 0, strings.Count(raw, ",")+1)
	for _, proxy := range strings.Split(raw, ",") {
		if proxy = strings.TrimSpace(proxy); proxy != "" {
			proxies = append(proxies, proxy)
		}
	}
	// Gin's default RemoteIPHeaders is {"X-Forwarded-For", "X-Real-IP"} and it falls
	// through to the second when the first is absent. Narrow it to X-Forwarded-For: that
	// is the header our nginx provably rewrites (proxy_set_header X-Forwarded-For
	// $proxy_add_x_forwarded_for), so it can never carry a value the client chose. The
	// nginx config lives on the VM and in no repo — if someone later drops its X-Real-IP
	// line, the default would happily accept a caller-supplied X-Real-IP from the trusted
	// bridge. One header, one owner, no fallback to trust.
	server.RemoteIPHeaders = []string{"X-Forwarded-For"}
	if err := server.SetTrustedProxies(proxies); err != nil {
		return err
	}
	common.SysLog("trusted proxies: " + strings.Join(proxies, ", "))
	return nil
}

// sessionCookieSecure reports whether the session cookie carries the Secure attribute,
// i.e. whether the browser is forbidden from ever sending it over cleartext HTTP.
//
// The default is false, matching upstream: new-api is deployable over plain HTTP, and a
// Secure cookie on such a host breaks login silently-ish — the browser accepts the
// Set-Cookie and then never sends it back, so password login, OAuth state validation and
// the passkey begin/finish pair all fail to persist a session.
//
// Gressio's own deployments pin SESSION_COOKIE_SECURE=true in the compose overlay
// (transwork/docker-compose.transwork.yml), which is where the HTTPS-only assumption
// belongs per Rule 4 — the same split TRUSTED_PROXIES uses: conservative default here,
// deployment-specific value in the overlay. There nginx terminates TLS and the app is
// published on 127.0.0.1 only, so no cleartext path to it exists.
//
// Unset and empty both mean "use the default". A set-but-unparseable value is an error
// rather than a fallback: common.GetEnvOrDefaultBool would log and return the default,
// which for `SESSION_COOKIE_SECURE=ture` on the HTTPS-only deployment means silently
// serving a non-Secure cookie to an operator who was trying to enable exactly that
// protection. Refusing to boot is the same choice setTrustedProxies makes for an invalid
// CIDR, and it cannot regress security unnoticed.
func sessionCookieSecure() (bool, error) {
	raw, ok := os.LookupEnv("SESSION_COOKIE_SECURE")
	if !ok {
		return false, nil
	}
	if raw = strings.TrimSpace(raw); raw == "" {
		return false, nil
	}
	secure, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("SESSION_COOKIE_SECURE=%q is not a boolean: %w", raw, err)
	}
	return secure, nil
}

func InjectUmamiAnalytics() {
	analyticsInjectBuilder := &strings.Builder{}
	if os.Getenv("UMAMI_WEBSITE_ID") != "" {
		umamiSiteID := os.Getenv("UMAMI_WEBSITE_ID")
		umamiScriptURL := os.Getenv("UMAMI_SCRIPT_URL")
		if umamiScriptURL == "" {
			umamiScriptURL = "https://analytics.umami.is/script.js"
		}
		analyticsInjectBuilder.WriteString("<script defer src=\"")
		analyticsInjectBuilder.WriteString(umamiScriptURL)
		analyticsInjectBuilder.WriteString("\" data-website-id=\"")
		analyticsInjectBuilder.WriteString(umamiSiteID)
		analyticsInjectBuilder.WriteString("\"></script>")
	}
	analyticsInjectBuilder.WriteString("<!--Umami QuantumNous-->\n")
	analyticsInject := []byte(analyticsInjectBuilder.String())
	placeholder := []byte("<!--umami-->\n")
	indexPage = bytes.ReplaceAll(indexPage, placeholder, analyticsInject)
	classicIndexPage = bytes.ReplaceAll(classicIndexPage, placeholder, analyticsInject)
}

func InjectGoogleAnalytics() {
	analyticsInjectBuilder := &strings.Builder{}
	if os.Getenv("GOOGLE_ANALYTICS_ID") != "" {
		gaID := os.Getenv("GOOGLE_ANALYTICS_ID")
		// Google Analytics 4 (gtag.js)
		analyticsInjectBuilder.WriteString("<script async src=\"https://www.googletagmanager.com/gtag/js?id=")
		analyticsInjectBuilder.WriteString(gaID)
		analyticsInjectBuilder.WriteString("\"></script>")
		analyticsInjectBuilder.WriteString("<script>")
		analyticsInjectBuilder.WriteString("window.dataLayer = window.dataLayer || [];")
		analyticsInjectBuilder.WriteString("function gtag(){dataLayer.push(arguments);}")
		analyticsInjectBuilder.WriteString("gtag('js', new Date());")
		analyticsInjectBuilder.WriteString("gtag('config', '")
		analyticsInjectBuilder.WriteString(gaID)
		analyticsInjectBuilder.WriteString("');")
		analyticsInjectBuilder.WriteString("</script>")
	}
	analyticsInjectBuilder.WriteString("<!--Google Analytics QuantumNous-->\n")
	analyticsInject := []byte(analyticsInjectBuilder.String())
	placeholder := []byte("<!--Google Analytics-->\n")
	indexPage = bytes.ReplaceAll(indexPage, placeholder, analyticsInject)
	classicIndexPage = bytes.ReplaceAll(classicIndexPage, placeholder, analyticsInject)
}

func InitResources() error {
	// Initialize resources here if needed
	// This is a placeholder function for future resource initialization
	err := godotenv.Load(".env")
	if err != nil {
		if common.DebugEnabled {
			common.SysLog("No .env file found, using default environment variables. If needed, please create a .env file and set the relevant variables.")
		}
	}

	// 加载环境变量
	common.InitEnv()

	logger.SetupLogger()

	// Initialize model settings
	ratio_setting.InitRatioSettings()

	service.InitHttpClient()

	service.InitTokenEncoders()

	// Initialize SQL Database
	err = model.InitDB()
	if err != nil {
		common.FatalLog("failed to initialize database: " + err.Error())
		return err
	}

	model.CheckSetup()

	// Initialize options, should after model.InitDB()
	model.InitOptionMap()

	if err := transwork.Init(); err != nil {
		common.SysLog("Warning: GCS client initialization failed (audio features will be disabled): " + err.Error())
	}

	// 清理旧的磁盘缓存文件
	common.CleanupOldCacheFiles()

	// 初始化模型
	model.GetPricing()

	// Initialize SQL Database
	err = model.InitLogDB()
	if err != nil {
		return err
	}

	// Initialize Redis
	err = common.InitRedisClient()
	if err != nil {
		return err
	}

	perfmetrics.Init()

	// 启动系统监控
	common.StartSystemMonitor()

	// Initialize i18n
	err = i18n.Init()
	if err != nil {
		common.SysError("failed to initialize i18n: " + err.Error())
		// Don't return error, i18n is not critical
	} else {
		common.SysLog("i18n initialized with languages: " + strings.Join(i18n.SupportedLanguages(), ", "))
	}
	// Register user language loader for lazy loading
	i18n.SetUserLangLoader(model.GetUserLanguage)

	// Load custom OAuth providers from database
	err = oauth.LoadCustomProviders()
	if err != nil {
		common.SysError("failed to load custom OAuth providers: " + err.Error())
		// Don't return error, custom OAuth is not critical
	}

	return nil
}
