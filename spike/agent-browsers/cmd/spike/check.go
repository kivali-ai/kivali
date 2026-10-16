package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

var botWallPhrases = []string{"just a moment", "access denied", "unusual traffic", "verify you are human", "captcha", "attention required"}

func appendFindings(text string) error {
	if err := os.MkdirAll("results", 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join("results", "findings.md"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	session := fs.String("session", "", "session name")
	target := fs.String("url", "", "account page URL that needs a sign-in")
	mode := fs.String("mode", "headless", "headless or xvfb")
	restarts := fs.Int("restarts", 3, "number of fresh-container restarts")
	_ = fs.Parse(args)
	if *session == "" || *target == "" {
		return fmt.Errorf("--session and --url are required")
	}
	prof := profilePath(*session)
	if _, err := os.Stat(prof); err != nil {
		return fmt.Errorf("no profile at %s: run `spike window --session %s` first", prof, *session)
	}
	_ = os.MkdirAll("results", 0o755)
	// Serve attack/ on 127.0.0.1:7788 too, so local test pages work.
	if srv, err := startHTTP(nil); err == nil {
		defer srv.Close()
	}

	if err := appendFindings(fmt.Sprintf("\n### check %s, mode %s, %s\n\nTarget: %s\n\n"+
		"| session | mode | restart | final URL | title | signed-in verdict | bot-wall verdict | webdriver | cookies for URL | profile MB | screenshot |\n"+
		"|---|---|---|---|---|---|---|---|---|---|---|\n",
		*session, *mode, time.Now().Format(time.RFC3339), *target)); err != nil {
		return err
	}

	for i := 1; i <= *restarts; i++ {
		fmt.Printf("restart %d/%d\n", i, *restarts)
		row, err := checkOnce(*session, *mode, *target, i)
		if err != nil {
			row = fmt.Sprintf("| %s | %s | %d | error: %s | | | | | | | |\n", *session, *mode, i, mdEscape(err.Error()))
			fmt.Println("  error:", err)
		}
		fmt.Print("  ", row)
		if err := appendFindings(row); err != nil {
			return err
		}
	}
	fmt.Println("rows appended to results/findings.md")
	return nil
}

func checkOnce(session, mode, target string, i int) (string, error) {
	b, err := StartBrowser(session, mode, profilePath(session))
	if err != nil {
		return "", err
	}
	defer b.Remove()

	var finalURL, title, bodyText string
	var hasPassword bool
	var shot []byte
	var cookies []*network.Cookie
	navCtx, cancel := context.WithTimeout(b.Ctx, 60*time.Second)
	navErr := chromedp.Run(navCtx, chromedp.Navigate(target))
	cancel()
	time.Sleep(3 * time.Second)
	evCtx, cancel := context.WithTimeout(b.Ctx, 30*time.Second)
	err = chromedp.Run(evCtx,
		chromedp.Evaluate(`location.href`, &finalURL),
		chromedp.Evaluate(`document.title`, &title),
		chromedp.Evaluate(`!!document.querySelector('input[type=password]')`, &hasPassword),
		chromedp.Evaluate(`(document.body ? document.body.innerText : '').slice(0, 50000)`, &bodyText),
		chromedp.CaptureScreenshot(&shot),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			cookies, err = network.GetCookies().Do(ctx)
			return err
		}),
	)
	cancel()
	if err != nil {
		return "", fmt.Errorf("inspect page: %w (navigate: %v)", err, navErr)
	}
	shotPath := filepath.Join("results", fmt.Sprintf("%s-%s-%d.png", session, mode, i))
	if err := os.WriteFile(shotPath, shot, 0o644); err != nil {
		return "", err
	}

	signinPath := false
	if u, err := url.Parse(finalURL); err == nil {
		p := strings.ToLower(u.Host + u.Path)
		for _, w := range []string{"login", "signin", "sign-in", "auth", "sso"} {
			if strings.Contains(p, w) {
				signinPath = true
			}
		}
	}
	var verdict string
	switch {
	case hasPassword && signinPath:
		verdict = "NOT signed in (password field + sign-in URL)"
	case hasPassword:
		verdict = "NOT signed in? (password field)"
	case signinPath:
		verdict = "NOT signed in? (sign-in URL)"
	default:
		verdict = "signed in (no sign-in form)"
	}
	hay := strings.ToLower(title + "\n" + bodyText)
	var walls []string
	for _, w := range botWallPhrases {
		if strings.Contains(hay, w) {
			walls = append(walls, w)
		}
	}
	wall := "none"
	if len(walls) > 0 {
		wall = "BOT WALL? " + strings.Join(walls, ", ")
	}
	if navErr != nil {
		wall += fmt.Sprintf(" (navigate error: %v)", navErr)
	}

	size, err := b.CloseAndPack(profilePath(session))
	if err != nil {
		return "", fmt.Errorf("repack: %w", err)
	}
	return fmt.Sprintf("| %s | %s | %d | %s | %s | %s | %s | %s | %d | %.2f | %s |\n",
		session, mode, i, mdEscape(finalURL), mdEscape(title), verdict, mdEscape(wall), b.Webdriver, len(cookies), mb(size), shotPath), nil
}

func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
