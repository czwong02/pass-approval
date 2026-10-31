package page

import (
	"html/template"
	"io"

	"pass-approval/internal/channel"
)

var layout = template.Must(template.New("page").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Pass</title>
{{if .Gone}}<p>This link is no longer active.</p>{{else}}
<h1>{{.Title}}</h1>
<p>{{.Body}}</p>
{{range .Fields}}<p><strong>{{.Label}}</strong> {{.Value}}</p>{{end}}
{{if .Buttons}}
<form method="post">
<button name="action" value="approve" type="submit">Approve</button>
<button name="action" value="reject" type="submit">Reject</button>
</form>
{{else}}<p>Status: {{.Status}}</p>{{end}}
{{end}}`))

type View struct {
	Gone    bool
	Title   string
	Body    string
	Fields  []channel.Field
	Status  string
	Buttons bool
}

func Write(w io.Writer, view View) error {
	return layout.Execute(w, view)
}
