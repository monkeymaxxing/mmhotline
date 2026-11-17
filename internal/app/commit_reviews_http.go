package app

import "net/http"

func (a *App) renderCommitReviewPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	review, err := a.CommitReview(r.Context(), p, r.PathValue("commit_id"))
	if err != nil {
		HTTPError(w, err)
		return
	}
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	title := review.Question()
	renderPage(w, status, title, frame, commitReviewHeading(title, review), commitReviewPageBody(frame, commitReviewPageData{review, a.CSRFToken(w, r), message}))
}

// commitReviewPageData is what a commit's review page shows (commit_reviews.templ).
type commitReviewPageData struct {
	Review      CommitReview
	CSRF, Error string
}

func (a *App) reviewCommitHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	if err := a.CheckBrowserMutation(w, r); err != nil {
		HTTPError(w, err)
		return
	}
	if err := a.ReviewCommit(r.Context(), p, r.PathValue("commit_id")); err != nil {
		a.renderCommitReviewPage(w, r, errorStatus(err), publicError(err))
		return
	}
	http.Redirect(w, r, "/approvals/commits/"+r.PathValue("commit_id"), http.StatusSeeOther)
}
