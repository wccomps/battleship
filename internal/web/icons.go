package web

import "html/template"

// iconPaths are the app's icons (24px grid, 2px stroke, drawn at 16px; see .i
// in app.css), inline SVG the CSP allows. They are decorative: adjacent words
// carry the meaning.
var iconPaths = map[string]string{
	"start":    `<path d="M7 4.5v15l12-7.5z"/>`,
	"shutdown": `<path d="M12 3v8M6.4 6.6a8 8 0 1 0 11.2 0"/>`,
	"stop":     `<rect x="6" y="6" width="12" height="12" rx="1.5"/>`,
	"reboot":   `<path d="M20 12a8 8 0 1 1-2.34-5.66"/><path d="M20 4v5h-5"/>`,
	"reset":    `<path d="M3 12a9 9 0 1 0 2.64-6.36L3 8"/><path d="M3 3v5h5"/><path d="M12 8v4l3 2"/>`,
	"snapshot": `<path d="M3 8h4l2-3h6l2 3h4v12H3z"/><circle cx="12" cy="13.5" r="3.5"/>`,
	"deploy":   `<path d="M12 3v12M7 10l5 5 5-5M4 21h16"/>`,
	"teardown": `<path d="M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3"/>`,
	"close":    `<path d="M6 6l12 12M18 6 6 18"/>`,
	"lock":     `<rect x="5" y="11" width="14" height="10" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>`,
	"blocked":  `<circle cx="12" cy="12" r="9"/><path d="M5.7 5.7l12.6 12.6"/>`,
	"warn":     `<path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/><path d="M12 9v4M12 17h.01"/>`,
	"stale":    `<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>`,
	"back":     `<path d="M19 12H5M11 6l-6 6 6 6"/>`,
	"done":     `<path d="M5 12.5 9.5 17 19 7.5"/>`,
	"wait":     `<circle cx="12" cy="12" r="8"/>`,
	"failed":   `<circle cx="12" cy="12" r="9"/><path d="m9 9 6 6M15 9l-6 6"/>`,
	"paused":   `<circle cx="12" cy="12" r="9"/><path d="M10 9v6M14 9v6"/>`,
	"dash":     `<path d="M7 12h10"/>`,
}

// icon is the SVG of a named icon, with extra classes if given.
func icon(name string, class ...string) template.HTML {
	p, ok := iconPaths[name]
	if !ok {
		return ""
	}
	cls := "i"
	for _, c := range class {
		cls += " " + c
	}
	return template.HTML(`<svg class="` + cls + `" viewBox="0 0 24 24" aria-hidden="true">` + p + `</svg>`)
}
