package auth

// Page is a page auth shows by itself: login errors and 401/403
// refusals. The renderer SetRender installs draws it.
type Page struct {
	Status   int    // HTTP status
	Title    string // short heading
	Message  string // plain text for the user: what happened and what to do
	Link     string // a local link to offer, e.g. "/auth/login"; may be empty
	LinkText string
}
