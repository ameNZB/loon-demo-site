package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ameNZB/loon/core"

	"github.com/ameNZB/loon-plugins/pluginapi"
)

//go:embed web/templates web/static
var webFS embed.FS

// web is the demo's host-side HTTP surface: templates, static assets,
// cookie-session auth, and the public pages (home/search/groups/login). A real
// host backs this with its session store + a users table; the demo signs an
// HMAC cookie over two in-memory users. It replaces the earlier X-Demo-User
// header hack for browsers (the header still works for curl).
type web struct {
	users  map[string]*core.User // by username
	byID   map[int64]*core.User
	secret []byte
	log    *slog.Logger
	tmpls  map[string]*template.Template // page name -> parsed (base + page)

	// usenet plugin capabilities, looked up on the extension registry after Boot.
	usenet      pluginapi.UsenetIndex
	usenetAdmin pluginapi.UsenetAdmin
}

func newWeb(users map[string]*core.User, secret []byte, log *slog.Logger) *web {
	byID := make(map[int64]*core.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	w := &web{users: users, byID: byID, secret: secret, log: log, tmpls: map[string]*template.Template{}}
	for _, page := range []string{"home.html", "groups.html", "search.html", "login.html", "admin_usenet.html"} {
		w.tmpls[page] = template.Must(template.ParseFS(webFS,
			"web/templates/base.html", "web/templates/"+page))
	}
	return w
}

// ── cookie session: an HMAC-signed user id ──────────────────────────

const sessionCookie = "loon_session"

func (w *web) sign(uid int64) string {
	payload := strconv.FormatInt(uid, 10)
	mac := hmac.New(sha256.New, w.secret)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (w *web) verify(v string) (int64, bool) {
	payload, sig, ok := strings.Cut(v, ".")
	if !ok {
		return 0, false
	}
	mac := hmac.New(sha256.New, w.secret)
	mac.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return 0, false
	}
	uid, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return 0, false
	}
	return uid, true
}

// currentUser resolves the request's user from the session cookie, falling back
// to the X-Demo-User header so the curl examples keep working.
func (w *web) currentUser(c *gin.Context) (*core.User, bool) {
	if ck, err := c.Cookie(sessionCookie); err == nil && ck != "" {
		if uid, ok := w.verify(ck); ok {
			if u := w.byID[uid]; u != nil {
				return u, true
			}
		}
	}
	if u, ok := w.users[c.GetHeader("X-Demo-User")]; ok {
		return u, true
	}
	return nil, false
}

// requireAtLeast gates a route on a minimum role. Unauthenticated browser
// requests are redirected to /login; API/curl requests get a 401.
func (w *web) requireAtLeast(min core.Role) gin.HandlersChain {
	return gin.HandlersChain{func(c *gin.Context) {
		u, ok := w.currentUser(c)
		if !ok {
			if strings.Contains(c.GetHeader("Accept"), "text/html") {
				c.Redirect(http.StatusSeeOther, "/login")
				c.Abort()
				return
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"ok": false, "error": "log in (or set X-Demo-User: alice)"})
			return
		}
		if u.Role < min {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"ok": false, "error": "insufficient role"})
			return
		}
		c.Next()
	}}
}

// requireRole is the exact-role variant used by core.AuthAdapter.RequireRoleFn.
func (w *web) requireRole(role core.Role) gin.HandlersChain {
	return gin.HandlersChain{func(c *gin.Context) {
		if u, ok := w.currentUser(c); !ok || u.Role != role {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"ok": false, "error": "wrong role"})
			return
		}
		c.Next()
	}}
}

// ── routes + rendering ──────────────────────────────────────────────

func (w *web) mount(e *gin.Engine) {
	sub, _ := fs.Sub(webFS, "web/static")
	e.StaticFS("/static", http.FS(sub))
	e.GET("/", w.home)
	e.GET("/groups", w.groups)
	e.GET("/search", w.search)
	e.GET("/nzb/:id", w.nzbDownload)
	e.GET("/login", w.loginPage)
	e.POST("/login", w.loginPost)
	e.GET("/logout", w.logout)
}

func (w *web) render(c *gin.Context, page string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	u, _ := w.currentUser(c)
	data["User"] = u
	data["Path"] = c.Request.URL.Path
	t := w.tmpls[page]
	if t == nil {
		c.String(http.StatusInternalServerError, "unknown page %q", page)
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(c.Writer, "base.html", data); err != nil {
		w.log.Error("render", "page", page, "err", err)
	}
}

func (w *web) home(c *gin.Context) {
	// News lands with the news plugin; the empty state renders until then.
	w.render(c, "home.html", map[string]any{"Title": "News"})
}

func (w *web) groups(c *gin.Context) {
	data := map[string]any{"Title": "Groups", "Configured": w.usenet != nil}
	if w.usenet != nil {
		if gs, err := w.usenet.Groups(c.Request.Context()); err == nil {
			data["Groups"] = gs
		}
	}
	w.render(c, "groups.html", data)
}

func (w *web) search(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	data := map[string]any{"Title": "Search", "Query": q, "Configured": w.usenet != nil}
	if q != "" && w.usenet != nil {
		if res, err := w.usenet.Search(c.Request.Context(), q, 50); err == nil {
			data["Results"] = toSearchRows(res)
		}
	}
	w.render(c, "search.html", data)
}

func (w *web) loginPage(c *gin.Context) {
	w.render(c, "login.html", map[string]any{"Title": "Log in"})
}

func (w *web) loginPost(c *gin.Context) {
	name := strings.TrimSpace(c.PostForm("username"))
	u, ok := w.users[name]
	if !ok {
		c.Status(http.StatusUnauthorized)
		w.render(c, "login.html", map[string]any{"Title": "Log in", "Error": "Unknown user — try alice or bob."})
		return
	}
	// 7-day cookie, HttpOnly. Secure=false because the demo runs on plain HTTP.
	c.SetCookie(sessionCookie, w.sign(u.ID), 7*24*3600, "/", "", false, true)
	c.Redirect(http.StatusSeeOther, "/")
}

func (w *web) logout(c *gin.Context) {
	c.SetCookie(sessionCookie, "", -1, "/", "", false, true)
	c.Redirect(http.StatusSeeOther, "/")
}

// usersAdapter builds the core.UsersService backing from the in-memory map.
func (w *web) usersAdapter() core.UsersAdapter {
	return core.UsersAdapter{
		GetByIDFn:       func(_ context.Context, id int64) (*core.User, error) { return w.byID[id], nil },
		GetByUsernameFn: func(_ context.Context, name string) (*core.User, error) { return w.users[name], nil },
		DisplayNameFn: func(_ context.Context, id int64) (string, error) {
			if u := w.byID[id]; u != nil {
				return u.Username, nil
			}
			return "", nil
		},
		BulkDisplayNamesFn: func(_ context.Context, ids []int64) (map[int64]string, error) {
			out := make(map[int64]string, len(ids))
			for _, id := range ids {
				if u := w.byID[id]; u != nil {
					out[id] = u.Username
				}
			}
			return out, nil
		},
	}
}
