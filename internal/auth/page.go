package auth

// Page is a page auth shows by itself: login errors and 401/403
// refusals. The renderer SetRender installs draws it.
type Page struct {
	Status   int    // HTTP status
	Title    string // short heading
	Message  string // plain text for the user: what happened and what to do
	Link     string // a local link to offer, e.g. "/auth/login"; may be empty
	LinkText string
	// Refresh, if set, is a local URL the page goes on to at once. The page
	// stays up while that request runs, so a slow step shows as progress.
	Refresh string
}
