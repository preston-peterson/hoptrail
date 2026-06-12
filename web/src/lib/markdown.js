// Minimal markdown → HTML renderer for the in-app docs viewer
// (step-143). Hand-rolled like the sibling project's — no library
// dependency. Covers what the four guides actually use: headings,
// paragraphs, bold/italic, inline code, fenced code blocks, links,
// nested-one-level lists, blockquotes, tables, hr. Everything is
// HTML-escaped first; the only emitted markup is our own.

function esc(s) {
  return s
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
}

// Inline spans: code first (its content is opaque), then bold, italic,
// links. Operates on already-escaped text.
function inline(s) {
  // `code`
  s = s.replace(/`([^`]+)`/g, (_, c) => `<code>${c}</code>`)
  // **bold**
  s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
  // *italic* (avoid ** leftovers)
  s = s.replace(/(^|[^*])\*([^*\n]+)\*/g, '$1<em>$2</em>')
  // [text](url) — .md links become doc-switch hooks the viewer
  // intercepts; external links open new tabs.
  s = s.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (_, text, url) => {
    if (/^[\w-]+\.md(#.*)?$/.test(url)) {
      return `<a href="#" data-doc="${url.replace(/\.md(#.*)?$/, '')}">${text}</a>`
    }
    return `<a href="${url}" target="_blank" rel="noopener noreferrer">${text}</a>`
  })
  return s
}

export function renderMarkdown(md) {
  const lines = md.split('\n')
  const out = []
  let i = 0
  let para = []

  const flushPara = () => {
    if (para.length) {
      out.push(`<p>${inline(esc(para.join(' ')))}</p>`)
      para = []
    }
  }

  while (i < lines.length) {
    const line = lines[i]

    // Fenced code block.
    if (/^```/.test(line)) {
      flushPara()
      const buf = []
      i++
      while (i < lines.length && !/^```/.test(lines[i])) {
        buf.push(esc(lines[i]))
        i++
      }
      i++ // closing fence
      out.push(`<pre><code>${buf.join('\n')}</code></pre>`)
      continue
    }

    // Heading.
    const h = line.match(/^(#{1,6})\s+(.*)$/)
    if (h) {
      flushPara()
      const lvl = h[1].length
      out.push(`<h${lvl}>${inline(esc(h[2]))}</h${lvl}>`)
      i++
      continue
    }

    // Horizontal rule.
    if (/^(\*{3,}|-{3,}|_{3,})\s*$/.test(line)) {
      flushPara()
      out.push('<hr>')
      i++
      continue
    }

    // Table: a header row followed by a |---| separator.
    if (/^\|.*\|\s*$/.test(line) && /^\|[\s:|-]+\|\s*$/.test(lines[i + 1] ?? '')) {
      flushPara()
      const cells = (row) => row.replace(/^\||\|\s*$/g, '').split('|').map((c) => inline(esc(c.trim())))
      const head = cells(line)
      i += 2
      const rows = []
      while (i < lines.length && /^\|.*\|\s*$/.test(lines[i])) {
        rows.push(cells(lines[i]))
        i++
      }
      out.push(
        '<table><thead><tr>' + head.map((c) => `<th>${c}</th>`).join('') + '</tr></thead><tbody>' +
        rows.map((r) => '<tr>' + r.map((c) => `<td>${c}</td>`).join('') + '</tr>').join('') +
        '</tbody></table>'
      )
      continue
    }

    // Blockquote (single level — that's all the docs use).
    if (/^>\s?/.test(line)) {
      flushPara()
      const buf = []
      while (i < lines.length && /^>\s?/.test(lines[i])) {
        buf.push(lines[i].replace(/^>\s?/, ''))
        i++
      }
      out.push(`<blockquote><p>${inline(esc(buf.join(' ')))}</p></blockquote>`)
      continue
    }

    // List (ul/ol, one nesting level via 2+ space indent).
    const li = line.match(/^(\s*)([-*]|\d+\.)\s+(.*)$/)
    if (li) {
      flushPara()
      const ordered = /\d+\./.test(li[2])
      const tag = ordered ? 'ol' : 'ul'
      const items = []
      while (i < lines.length) {
        const m = lines[i].match(/^(\s*)([-*]|\d+\.)\s+(.*)$/)
        if (m && m[1].length === li[1].length) {
          items.push({ text: m[3], subs: [] })
          i++
          // Continuation lines + nested items.
          while (i < lines.length) {
            const sub = lines[i].match(/^(\s+)([-*]|\d+\.)\s+(.*)$/)
            if (sub && sub[1].length > li[1].length) {
              items[items.length - 1].subs.push(sub[3])
              i++
            } else if (/^\s{2,}\S/.test(lines[i]) && !lines[i].match(/^(\s*)([-*]|\d+\.)\s+/)) {
              items[items.length - 1].text += ' ' + lines[i].trim()
              i++
            } else break
          }
        } else break
      }
      out.push(
        `<${tag}>` + items.map((it) => {
          let h = `<li>${inline(esc(it.text))}`
          if (it.subs.length) h += '<ul>' + it.subs.map((st) => `<li>${inline(esc(st))}</li>`).join('') + '</ul>'
          return h + '</li>'
        }).join('') + `</${tag}>`
      )
      continue
    }

    // Blank line ends a paragraph.
    if (/^\s*$/.test(line)) {
      flushPara()
      i++
      continue
    }

    para.push(line.trim())
    i++
  }
  flushPara()
  return out.join('\n')
}
