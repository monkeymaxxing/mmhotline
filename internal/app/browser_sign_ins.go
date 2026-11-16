package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// Ten minutes limits exposure of a link printed in a terminal that could be copied.
const signInLinkLifetime = 10 * time.Minute

// Twelve hours covers one long incident without running the command again.
const signInCookieLifetime = 12 * time.Hour
const signInCookieName = "__Host-mmhotline-sign-in"

func (a *App) sshForwardCommand() (string, string) {
	host, port, _ := net.SplitHostPort(a.WebListenAddress)
	return port, fmt.Sprintf("ssh -L %s:%s YOUR_SSH_HOST", port, net.JoinHostPort(host, port))
}

func (a *App) createSignInLink(ctx context.Context, principal Principal, address string, result *CommandResult) error {
	if principal.UnixUser == "" {
		return ErrForbidden
	}
	address, err := normalizeSignInEmailAddress(address)
	if err != nil {
		return err
	}
	port, forward := a.sshForwardCommand()
	token := randomToken()
	expires := a.Now().Add(signInLinkLifetime).UTC()
	var member Member
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		member, err = personBySignInEmailAddress(ctx, tx, address)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM browser_sign_ins WHERE expires_at<=?`, a.Now().Unix()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO browser_sign_ins(token_sha256,member_id,kind,expires_at) VALUES(?,?,'one_time_link',?)`, tokenHash(token), member.ID, expires.Unix())
		return err
	})
	if err != nil {
		return err
	}
	result.SignInLinkURL = "http://localhost:" + port + "/sign-in/" + token
	result.SignInLinkValidUntil = expires.Format(time.RFC3339)
	result.SSHForwardCommand = forward
	result.Message = "This sign-in link works once."
	log.Printf("Created sign-in link for %s (%s)", member.Name, member.SignInEmailAddress)
	return nil
}

func (a *App) signInLinkHTTP(w http.ResponseWriter, r *http.Request) {
	token := randomToken()
	now := a.Now()
	err := a.Store.WithTransaction(r.Context(), func(tx *sql.Tx) error {
		var memberID string
		err := tx.QueryRowContext(r.Context(), `SELECT s.member_id FROM browser_sign_ins s JOIN members m ON m.id=s.member_id WHERE s.token_sha256=? AND s.kind='one_time_link' AND s.expires_at>? AND m.removed_at IS NULL`, tokenHash(r.PathValue("token")), now.Unix()).Scan(&memberID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(r.Context(), `DELETE FROM browser_sign_ins WHERE token_sha256=?`, tokenHash(r.PathValue("token"))); err != nil {
			return err
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO browser_sign_ins(token_sha256,member_id,kind,expires_at) VALUES(?,?,'sign_in_cookie',?)`, tokenHash(token), memberID, now.Add(signInCookieLifetime).Unix())
		return err
	})
	if errors.Is(err, ErrUnauthorized) {
		log.Print("Sign-in link refused: the link was already used or has expired")
		RenderPage(w, 401, "Sign-in link unavailable", `<section class="panel"><div class="panel-body"><p>This sign-in link was already used or has expired.</p><p>Ask someone with SSH access to the server for a new sign-in link.</p><details class="fold"><summary>Create a new sign-in link</summary><p>On the server, run <code>sudo mmhotline people create-sign-in-link --sign-in-email-address ADDRESS</code>, using your sign-in email address. Follow the command's SSH forwarding instructions, then open the new link.</p></details><a href="/dashboard">Try the dashboard again</a></div></section>`, true)
		return
	}
	if err != nil {
		HTTPError(w, err)
		return
	}
	name, secure := browserCookie(r, signInCookieName)
	http.SetCookie(w, &http.Cookie{Name: name, Value: token, Secure: secure, HttpOnly: true, SameSite: http.SameSiteStrictMode, Path: "/", MaxAge: int(signInCookieLifetime.Seconds())})
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (a *App) cookiePrincipal(r *http.Request) (Principal, error) {
	name, _ := browserCookie(r, signInCookieName)
	cookie, err := r.Cookie(name)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	var principal Principal
	err = a.Store.db.QueryRowContext(r.Context(), `SELECT m.sign_in_email_address FROM browser_sign_ins s JOIN members m ON m.id=s.member_id WHERE s.token_sha256=? AND s.kind='sign_in_cookie' AND s.expires_at>? AND m.removed_at IS NULL`, tokenHash(cookie.Value), a.Now().Unix()).Scan(&principal.SignInEmailAddress)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	return principal, err
}
