# snugbox.dev

The landing page and product documentation are static HTML with no JavaScript.
`build.py` writes them to `public/`; Caddy serves the result (see `server/`).

    py -3 -m pip install -r requirements.txt
    py -3 build.py              # build public/
    pwsh -File deploy.ps1       # build and make it live

- `src/index.html`, `src/404.html`: the pages. `{{CSS}}`, `{{FONT}}` and
  `{{SPRITE}}` are filled in by the build.
- `src/doc.html` and `src/docs.md` render the documentation index and Markdown in `../docs/`. The same build publishes the audit JSON schema.
- `src/extra.css`: the site's own styles. `src/design.css`, `src/sprite.svg`,
  `src/licenses.txt` and `src/icons/` come from the design preview with
  `build.py --from-preview <file>`; don't edit them by hand.
- `src/og.png`: the 1200 x 630 card shown when the link is shared. To redraw
  it, run `build.py --og`, serve `public/` and screenshot `/_og.html`.
- `fonts/`: Recursive, under the SIL Open Font License (`fonts/OFL.txt`).

The build stops if a page has an inline style or a script, because the
server's Content-Security-Policy forbids both, or if a class has no CSS rule.
