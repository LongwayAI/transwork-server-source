package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// TopupResult serves a self-contained, public "payment result" page that Stripe
// Checkout redirects the browser to after a top-up. Desktop users land here
// instead of the internal admin dashboard. It reads an optional ?status= query
// param: only an explicit "success" renders the success page; "cancel" and any
// unknown/empty status render the neutral cancel page, so a stray or malformed
// visit never shows a misleading "Payment complete".
func TopupResult(c *gin.Context) {
	switch c.Query("status") {
	case "success":
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(topupSuccessPage))
	default:
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(topupCancelPage))
	}
}

const topupSuccessPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Payment complete / 支付成功 · Gressio</title>
<style>
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif;background:#f6f8fa;color:#1f2328}
.card{max-width:420px;padding:40px 32px;background:#fff;border-radius:16px;box-shadow:0 6px 24px rgba(0,0,0,.08);text-align:center}
.icon{width:64px;height:64px;border-radius:50%;background:#dcfce7;color:#16a34a;display:flex;align-items:center;justify-content:center;font-size:34px;margin:0 auto 20px}
h1{font-size:22px;margin:0 0 6px}
h2{font-size:18px;font-weight:600;margin:0 0 18px;color:#444}
p{font-size:15px;line-height:1.6;margin:6px 0;color:#57606a}
</style>
</head>
<body>
<div class="card">
<div class="icon">&#10003;</div>
<h1>Payment complete</h1>
<h2>支付成功</h2>
<p>You can close this tab and return to Gressio.</p>
<p>您可以关闭此页面并返回 Gressio。</p>
</div>
</body>
</html>`

const topupCancelPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Payment canceled / 支付已取消 · Gressio</title>
<style>
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif;background:#f6f8fa;color:#1f2328}
.card{max-width:420px;padding:40px 32px;background:#fff;border-radius:16px;box-shadow:0 6px 24px rgba(0,0,0,.08);text-align:center}
.icon{width:64px;height:64px;border-radius:50%;background:#fef3c7;color:#d97706;display:flex;align-items:center;justify-content:center;font-size:34px;margin:0 auto 20px}
h1{font-size:22px;margin:0 0 6px}
h2{font-size:18px;font-weight:600;margin:0 0 18px;color:#444}
p{font-size:15px;line-height:1.6;margin:6px 0;color:#57606a}
</style>
</head>
<body>
<div class="card">
<div class="icon">&#9888;</div>
<h1>Payment canceled</h1>
<h2>支付已取消</h2>
<p>No charge was made. You can retry the top-up from within Gressio.</p>
<p>本次未扣款。您可以在 Gressio 应用中重新发起充值。</p>
</div>
</body>
</html>`
