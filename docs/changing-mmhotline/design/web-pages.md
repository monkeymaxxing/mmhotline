# Web pages in templ

MMHotline's pages are written in [templ](https://templ.guide). templ
compiles each template to Go, so a broken page or a wrong field name fails
the build instead of a request. Go's `html/template` finds those faults only
when the page is rendered. The [interface](interface.md) design owns what the
pages show; this document owns how a page is written.

## How a page is written

- **One `.templ` file beside the handler.** The People page's handler is in
  `internal/app/people_http.go`, and its body is in `people.templ`. Name the
  file after the handler's file without `_http`. A handler file with several
  pages, such as `dashboard.go`, has one `.templ` file for all of them.
- **Typed by the view struct.** A page's component takes the page's view
  struct, such as `peoplePageBody(data peoplePageData)`. The handler fills
  the struct as before.
- **Generated files are committed.** `go tool templ generate`, run from the
  repository root, writes `<name>_templ.go` beside each `.templ` file. The
  version is pinned by the `tool` line in `go.mod`. Commit the generated
  files with the template, and never edit them. The checks workflow
  regenerates them and fails when the result differs from the commit.
- **The handler reads the frame, then renders.** After reading its data:

  ```go
  frame, ok := readFrameForPage(w)
  if !ok {
  	return
  }
  renderPage(w, 200, "People", frame, nil, peoplePageBody(data))
  ```

  `readFrameForPage` reads the menu, the red bar, and the time zone. When
  the read fails, it has already written the error page.
  `renderPage(w, status, title, frame, heading, body)` writes the whole page
  in the frame, the `pageLayout` component in `layout.templ`. A `nil`
  heading gives the frame's `<h1>` with the title. A page with breadcrumbs,
  tags, or a meta line passes its own heading component, which takes the
  title as a parameter. `renderPage` sets the status code and the
  `Content-Type` header. When rendering fails, `renderPage` writes the error
  page instead of half a page.
- **The golden test pins every page.** `TestGoldenPages` in
  `golden_pages_test.go` renders each page through its real handler and
  compares the page with its file under `testdata/pages/app/`.
  [Package htmlgolden](../../../internal/htmlgolden/htmlgolden.go) lists
  the differences the comparison ignores. Run the test after every page
  change: `go test ./internal/app -run TestGoldenPages -count=1`.

### Helpers

Plain Go functions are called in `{ }` expressions:

| In a template | Writes |
| --- | --- |
| `{ frame.Clock(t) }` | A time, to the minute, in the team's time zone (`pageClock`) |
| `{ frame.RelativeTime(t) }` | "8 minutes ago", "in 2 minutes" (`relativeTime`) |
| `{ capitalize(s) }`, `{ strings.ToLower(s) }` | Text with the first letter changed, or all letters lowercase |
| `{ minutesText(d) }`, `{ durationText(d) }` | "5 minutes"; "1 minute 30 seconds" |
| `{ countText(n, "person", "people") }` | "1 person", "3 people" |
| `{ severityLabel(v) }`, `{ severityMeaning(v) }` | "Warning"; "needs attention, but nobody is woken" |
| `{ scheduleTypeLabel(v) }`, `{ incidentStatusLabel(v) }` | "Weekly rotation"; "Not claimed yet" |
| `@withBreaksAfterSeparators(s)` | A value such as `app002/disk/root_disk_usage` with a `<wbr>` after each `/` and `_`, so the value can wrap |
| `@settingsFileNoticeLine(data.settingsFileNoticeView) { <code>people</code> }` | The line saying where a page's read-only settings are set; the children name the keys |

A page body that needs the time passes `frame` to its component, such as
`dashboardBody(frame, data)`.

## Writing templ that matches the old pages

Until the last page moves, `RenderPage(w, status, title, body, data)` still
renders an `html/template` body. `RenderPage` renders the heading and the
body with `html/template`, then hands the HTML to `renderPage`, so both kinds
of page share one frame. A converted page deletes its template string and
calls `renderPage`. The last conversion deletes `RenderPage`, the
`settingsFileNotice` string, `breakAfterSeparators`, and this section's
translation table.

| `html/template` | templ |
| --- | --- |
| `{{.Name}}` | `{ data.Name }`; strings, numbers, and booleans are allowed |
| `{{if .X}}` | `if data.X != "" {`: say the condition. `html/template` treats `""`, `0`, `nil`, `false`, and an empty slice or map as false |
| `{{with .X}}…{{.}}…{{end}}` | `if x := data.X; x != nil {` … `{ x.Title }` … `}` |
| `{{range .Items}}…{{else}}…{{end}}` | `for _, item := range data.Items {` … `}`, then `if len(data.Items) == 0 {` … `}` |
| `{{$page := .}}`, `$.CSRF` | the component's parameter: `data.CSRF` |
| `{{template "x" .}}` with `{{define "x"}}` | a component `templ x(v T)` in the same `.templ` file, called as `@x(v)` |
| `{{define "heading"}}` | a component such as `alertSourceHeading(title string, data alertSourcePageData)`, passed as `renderPage`'s heading |
| `eq`, `ne`, `not`, `and`, `or`, `len`, `printf` | `==`, `!=`, `!`, `&&`, `\|\|`, `len()`, `fmt.Sprintf` |
| `{{clock .At}}`, `{{relativeTime .At}}` | `{ frame.Clock(data.At) }`, `{ frame.RelativeTime(data.At) }` |
| `{{breakAfterSeparators .Key}}` | `@withBreaksAfterSeparators(data.Key)` |
| A Go string joined into the template, such as `settingsFileNotice(...)` | a component, or `{ constName }` for plain text |
| A `template.HTML` field in the view struct | a component. Use `templ.Raw` only for HTML that is already escaped, with a comment saying where the HTML comes from |

### Spaces between inline elements

templ removes the whitespace between nodes, with one exception: between two
inline neighbors, any whitespace or line break in the `.templ` file becomes
one space. Inline neighbors are text, a `{ }` expression, an inline element
such as `<a>`, `<span>`, `<strong>`, `<code>`, `<small>`, or `<wbr>`, and an
`if`, `for`, or `switch`. Block elements such as `<div>`, `<p>`, `<li>`, and
`<section>` never get the space. A space between two inline neighbors
shows on the page, so the golden comparison catches a missing or extra one:

- **No space wanted:** write the neighbors on one line with nothing between
  them: `<span class="mark" aria-hidden="true">MM</span><span>MMHotline</span>`.
- **An `if` adds a space even when its condition is false.** In
  `<a>{ link.Label }`, then `if link.Count > 0 {` on the next line, the
  link's text gets a trailing space whenever the count is zero. Put the
  whole inline run in each branch instead, as `sideMenu` in `layout.templ`
  does.
- **`go tool templ fmt` can add a space.** The formatter can move an inline
  element onto its own line. Run the golden test after formatting.

### URL attributes

templ sanitizes the value of `href`, `action`, `formaction`, and `src` with
`templ.URL`, then escapes the value for HTML. Pass a plain `string`. Do not
wrap the value in `templ.SafeURL`, which turns the sanitizing off.

templ's result differs from `html/template`'s in two ways:

- **Encoding.** `html/template` percent-encodes characters that are not
  allowed in a URL: `/x y/ü` became `/x%20y/%c3%bc`, and `"`, `<`, and `>`
  became `%22`, `%3c`, and `%3e`. templ writes the characters as they are,
  HTML-escaped.
- **Schemes.** Both keep relative URLs and `http`, `https`, and `mailto`
  URLs. `html/template` replaced any other scheme with `#ZgotmplZ`. templ
  also allows `tel`, `ftp`, and `ftps`, and replaces other schemes with
  `about:invalid#TemplFailedSanitizationURL`.

So a URL comes out the same when the URL holds no space, non-ASCII
character, or quote. Record IDs, fixed codes such as a sending service's
name, and URLs built with `net/url` qualify. For each URL value from data,
check where the value comes from: `.URL`, `.ReviewURL`, `.WebURL`,
`.PageURL`, `.Target`, `.TakenOverLink`, `.Next`, `.Link`, `.AppURL`, and the
path parts such as `/approvals/{ID}`. Encode a value that may hold such a
character in Go, with `url.PathEscape` or `url.QueryEscape`.

The golden comparison compares attribute values whole, so the comparison
catches a changed URL only when the fixture data has such a character. Today
only the Slack setup page has percent-encoded characters, in a link built
already encoded.

### Live sections

`refresh.js` replaces each element that has a `data-live` attribute and an
`id` when the element's content changes. Keep both attributes and the `id`
unchanged. `data-live` is written bare: `<section id="on-call-now"
data-live class="panel">`. A conditional attribute goes inside the tag:

```templ
<section
	id="commit-reviews"
	data-live
	if len(data.Commits) > 0 {
		class="panel warn"
	} else {
		hidden
	}
>
```

## Parts other apps can reuse

The website app will reuse the frame's look. These parts are general:

- **Layout components** in `layout.templ`: `pageLayout` (head, stylesheet
  link, shell, side menu, phone top bar, heading, body, footer), `brand`, and
  `sideMenu` with its groups and counts (`menuGroup`, `menuLink`).
- **Stylesheet classes** in `mmhotline.css`: the frame (`.shell`, `.side`,
  `.topbar`, `.menu`, `.content`, `.wrap`, `.brand`, `.mark`), the menu
  (`.nav`, `.group`, `.count`), the heading (`.head`, `.meta`, `.crumbs`,
  `.tags`), layout (`.stack`, `.row`, `.cols`, `.col-main`, `.col-side`),
  `.panel` with `.panel-head`, `.panel-body`, `.panel-foot`, and
  `.panel.quiet`, forms (`.field`, `.form-error`), `.btn` with `.primary`,
  `.quiet`, `.small`, `.big`, and `.block`, `.tag` with `.ok`, `.warn`,
  `.sos`, `.work`, and `.nodot`, text (`.small`, `.muted`, `.faint`),
  disclosures (`details.more`, `details.fold`), `.kv` lists, `.chips`,
  `.table`, and the color tokens (`--bg`, `--surface`, `--text`, `--link`,
  `--accent`, `--sos`, `--warn`, `--ok`, `--work`, and their variants) for
  light and dark.

These parts are MMHotline-only: the red bar (`waitingSOSStrip`, `.strip`),
the development-mode notice, `settingsFileNoticeLine`, and the classes of
single pages, such as `.oncall-*`, `.incident-*`, `.approval*`,
`.triage-*`, `.contact-*`, `.settings-*`, `.timeline`, and `.countdown`.

The stylesheet stays one file, `internal/app/mmhotline.css`. Splitting it
waits for the website app's first page, which shows which rules that page
needs.
