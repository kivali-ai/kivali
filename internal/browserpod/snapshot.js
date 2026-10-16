// The page as a web subagent reads it. Evaluated in the pod's isolated
// world; returns {title, url, viewport, lines, refs}. Each line is
// [depth, text]; interactive elements carry a ref "eN", and the element
// is kept in window.__kivaliRefs[N] for the commands that act on it.
(() => {
  const MAX_TEXT = 300, MAX_NAME = 120, MAX_ALT = 80, MAX_VALUE = 100, MAX_LINES = 20000;
  const SKIP = new Set(['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE', 'HEAD', 'META', 'LINK', 'OPTION', 'OPTGROUP', 'DATALIST', 'AREA', 'MAP', 'SLOT']);
  const ROLES = new Set(['button', 'link', 'tab', 'menuitem', 'option', 'checkbox', 'switch', 'textbox', 'combobox', 'radio', 'menuitemcheckbox', 'menuitemradio', 'searchbox', 'slider', 'spinbutton', 'treeitem']);
  const LANDMARKS = {NAV: 'navigation', MAIN: 'main', HEADER: 'banner', FOOTER: 'contentinfo', ASIDE: 'complementary', SECTION: 'region', FORM: 'form', SEARCH: 'search'};
  const LANDMARK_ROLES = new Set(['region', 'navigation', 'main', 'banner', 'contentinfo', 'complementary', 'form', 'search']);
  const INLINE = new Set(['inline', 'contents']);

  const refs = [null];
  const refInfo = [null];
  const docEl = document.documentElement;
  const docWidth = Math.max(docEl.scrollWidth, document.body ? document.body.scrollWidth : 0);
  const sx = window.scrollX, sy = window.scrollY;

  const clean = s => (s || '').replace(/\s+/g, ' ').trim();
  const cut = (s, n) => s.length > n ? s.slice(0, n - 1) + '…' : s;
  const q = s => JSON.stringify(s);

  const styles = new Map();
  const style = el => {
    let cs = styles.get(el);
    if (!cs) { cs = getComputedStyle(el); styles.set(el, cs); }
    return cs;
  };

  const transparent = c => !c || c === 'transparent' || /^rgba\(.*,\s*0\)$/.test(c);

  // The background a text node is drawn on: the nearest ancestor with
  // an opaque background, white when none has one.
  const bgCache = new Map();
  const background = el => {
    if (!el || el.nodeType !== 1) return 'rgb(255, 255, 255)';
    if (bgCache.has(el)) return bgCache.get(el);
    const bg = style(el).backgroundColor;
    const v = transparent(bg) ? background(el.parentElement) : bg;
    bgCache.set(el, v);
    return v;
  };

  const offDocument = r => r.right + sx < 0 || r.bottom + sy < 0 || r.left + sx > docWidth;

  const srOnly = (cs, r) => {
    const clipped = cs.clipPath === 'inset(100%)' || cs.clipPath === 'inset(50%)' ||
      /^rect\(0(px)?,? 0(px)?,? 0(px)?,? 0(px)?\)$/.test(cs.clip);
    return clipped && r.width <= 1 && r.height <= 1;
  };

  // hidden reports an element that is not shown, with everything in it.
  const hidden = el => {
    if (el.hidden || el.getAttribute('aria-hidden') === 'true') return true;
    const cs = style(el);
    if (cs.display === 'none' || cs.visibility === 'hidden' || cs.visibility === 'collapse') return true;
    if (parseFloat(cs.opacity) === 0) return true;
    if (cs.display === 'contents') return false;
    const r = el.getBoundingClientRect();
    if (srOnly(cs, r)) return true;
    if (r.width === 0 || r.height === 0) {
      // A zero-size box clips what it holds when it hides overflow;
      // otherwise its children may still show (inline containers).
      if (cs.overflow !== 'visible' || cs.overflowX !== 'visible' || cs.overflowY !== 'visible') return true;
    } else if (offDocument(r)) {
      return true;
    }
    return false;
  };

  // textShown reports whether a text node can be seen: it has a box in
  // the document, a font size, and a colour unlike its background.
  const textShown = node => {
    const el = node.parentElement;
    if (!el) return false;
    const cs = style(el);
    if (parseFloat(cs.fontSize) === 0) return false;
    if (transparent(cs.color)) return false;
    if (cs.color === background(el)) return false;
    const range = document.createRange();
    range.selectNodeContents(node);
    const r = range.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) return false;
    if (offDocument(r)) return false;
    return true;
  };

  const textOf = el => {
    const parts = [];
    const walk = n => {
      if (n.nodeType === 3) { if (textShown(n)) parts.push(n.nodeValue); return; }
      if (n.nodeType !== 1 || SKIP.has(n.tagName) || hidden(n)) return;
      if (n.tagName === 'IMG') { const a = n.getAttribute('alt'); if (a) parts.push(a); return; }
      if (n.tagName === 'svg' || n.tagName === 'SVG') { const t = svgName(n); if (t) parts.push(t); return; }
      for (const c of n.childNodes) walk(c);
      if (style(n).display !== 'inline') parts.push(' ');
    };
    for (const c of el.childNodes) walk(c);
    return clean(parts.join(''));
  };

  const byIds = ids => clean(ids.split(/\s+/).map(id => {
    const e = document.getElementById(id);
    return e ? e.textContent : '';
  }).join(' '));

  const svgName = el => {
    const label = el.getAttribute('aria-label');
    if (label) return clean(label);
    const t = el.querySelector('title');
    return t ? clean(t.textContent) : '';
  };

  const inputRole = el => {
    const t = (el.getAttribute('type') || 'text').toLowerCase();
    switch (t) {
      case 'checkbox': return 'checkbox';
      case 'radio': return 'radio';
      case 'file': return 'file';
      case 'submit': case 'button': case 'reset': case 'image': return 'button';
      case 'range': return 'slider';
      case 'number': return 'spinbutton';
      case 'search': return 'searchbox';
      default: return 'textbox';
    }
  };

  // role is the interactive role of an element, or '' when it is not
  // something to act on.
  const role = el => {
    const tag = el.tagName;
    const aria = (el.getAttribute('role') || '').toLowerCase();
    if (ROLES.has(aria)) return aria;
    if (tag === 'A' && el.hasAttribute('href')) return 'link';
    if (tag === 'BUTTON') return 'button';
    if (tag === 'INPUT') return el.type === 'hidden' ? '' : inputRole(el);
    if (tag === 'SELECT') return el.multiple || el.size > 1 ? 'listbox' : 'combobox';
    if (tag === 'TEXTAREA') return 'textbox';
    if (tag === 'SUMMARY') return 'button';
    if (el.isContentEditable && !(el.parentElement && el.parentElement.isContentEditable)) return 'textbox';
    if (el.hasAttribute('onclick')) return 'clickable';
    const ti = el.getAttribute('tabindex');
    if (ti !== null && parseInt(ti, 10) >= 0) return 'clickable';
    return '';
  };

  const name = (el, r) => {
    const lb = el.getAttribute('aria-labelledby');
    if (lb) { const n = byIds(lb); if (n) return cut(n, MAX_NAME); }
    const al = el.getAttribute('aria-label');
    if (al && clean(al)) return cut(clean(al), MAX_NAME);
    const tag = el.tagName;
    if (tag === 'INPUT' || tag === 'SELECT' || tag === 'TEXTAREA') {
      if (el.labels && el.labels.length) {
        const n = clean(Array.from(el.labels).map(l => textOf(l)).join(' '));
        if (n) return cut(n, MAX_NAME);
      }
      if (tag === 'INPUT' && ['submit', 'button', 'reset'].includes(el.type) && el.value) return cut(clean(el.value), MAX_NAME);
      if (tag === 'INPUT' && el.type === 'image' && el.alt) return cut(clean(el.alt), MAX_NAME);
      if (el.placeholder) return cut(clean(el.placeholder), MAX_NAME);
    } else {
      const n = textOf(el);
      if (n) return cut(n, MAX_NAME);
    }
    const title = el.getAttribute('title');
    if (r && title) return cut(clean(title), MAX_NAME);
    return '';
  };

  const shortHref = href => {
    try {
      const u = new URL(href, location.href);
      if (u.origin === location.origin) return cut(u.pathname + u.search + u.hash, MAX_VALUE);
      return cut(u.href, MAX_VALUE);
    } catch (e) { return cut(href, MAX_VALUE); }
  };

  // interactiveLine describes an element to act on and gives it a ref.
  const interactiveLine = (el, r) => {
    refs.push(el);
    const n = name(el, r);
    refInfo.push({role: r, name: n});
    let line = '[e' + (refs.length - 1) + '] ' + r + (n ? ' ' + q(n) : '');
    const tag = el.tagName;
    if (r === 'link' && tag === 'A') line += ' → ' + shortHref(el.getAttribute('href'));
    if (tag === 'INPUT' && (r === 'textbox' || r === 'searchbox' || r === 'spinbutton' || r === 'slider')) {
      line += ' value=' + q(el.type === 'password' ? (el.value ? '•'.repeat(Math.min(el.value.length, 8)) : '') : cut(el.value, MAX_VALUE));
      if (el.placeholder && el.placeholder !== n) line += ' placeholder=' + q(cut(clean(el.placeholder), MAX_VALUE));
    }
    if (tag === 'TEXTAREA') {
      line += ' value=' + q(cut(clean(el.value), MAX_VALUE));
      if (el.placeholder && el.placeholder !== n) line += ' placeholder=' + q(cut(clean(el.placeholder), MAX_VALUE));
    }
    if (tag === 'INPUT' && el.type === 'file') {
      line += el.files && el.files.length ? ' files=' + q(Array.from(el.files).map(f => f.name).join(', ')) : ' empty';
    }
    if (tag === 'SELECT') {
      const sel = Array.from(el.selectedOptions);
      line += ' value=' + q(sel.length ? sel[0].value : '');
      if (sel.length && clean(sel[0].label || sel[0].text) !== sel[0].value) line += ' selected=' + q(cut(clean(sel[0].label || sel[0].text), MAX_VALUE));
      line += ' options=' + el.options.length;
    }
    if (el.isContentEditable && tag !== 'INPUT' && tag !== 'TEXTAREA') {
      line += ' value=' + q(cut(clean(el.innerText), MAX_VALUE));
    }
    const ariaChecked = el.getAttribute('aria-checked');
    if ((tag === 'INPUT' && (el.type === 'checkbox' || el.type === 'radio') && el.checked) || ariaChecked === 'true') line += ' checked';
    if (el.getAttribute('aria-selected') === 'true') line += ' selected';
    const exp = el.getAttribute('aria-expanded');
    if (exp === 'true') line += ' expanded';
    if (exp === 'false') line += ' collapsed';
    if (el.disabled || el.getAttribute('aria-disabled') === 'true') line += ' disabled';
    if (el.required) line += ' required';
    return line;
  };

  // An item is {line, kids} for an element worth a line, {text} for a
  // run of text, or BREAK where a block boundary keeps text runs apart.
  const BREAK = {brk: true};

  const pushText = (out, s) => {
    const last = out[out.length - 1];
    if (last && last.text !== undefined) last.text += s;
    else out.push({text: s});
  };

  const append = (out, items) => {
    for (const it of items) {
      if (it.text !== undefined) pushText(out, it.text);
      else out.push(it);
    }
  };

  const node = (line, kids) => ({line, kids: finish(kids)});

  // finish turns text runs into lines and drops breaks.
  const finish = items => {
    const out = [];
    for (const it of items) {
      if (it === BREAK) continue;
      if (it.text !== undefined) {
        const t = clean(it.text);
        if (t) out.push({line: 'text ' + q(cut(t, MAX_TEXT)), kids: []});
        continue;
      }
      out.push(it);
    }
    return out;
  };

  // titled puts a lone text child on its parent's line.
  const titled = (label, kids) => {
    const f = finish(kids);
    if (f.length === 1 && f[0].line.startsWith('text ') && f[0].kids.length === 0) {
      return {line: label + ' ' + f[0].line.slice(5), kids: []};
    }
    return {line: label, kids: f};
  };

  let count = 0;

  const visit = el => {
    if (++count > MAX_LINES * 4) return [];
    const tag = el.tagName;
    if (SKIP.has(tag) || hidden(el)) return [];
    if (tag === 'svg' || tag === 'SVG') {
      const n = svgName(el);
      return n ? [{line: 'img ' + q(cut(n, MAX_ALT)), kids: []}] : [];
    }
    if (el.namespaceURI !== 'http://www.w3.org/1999/xhtml' && el.namespaceURI !== null) return [];
    if (tag === 'IFRAME' || tag === 'FRAME') {
      let label = el.getAttribute('title') || '';
      if (!label) { try { label = new URL(el.src, location.href).host; } catch (e) { label = ''; } }
      return [{line: 'iframe ' + q(cut(clean(label), MAX_NAME)), kids: []}];
    }
    if (tag === 'IMG') {
      const alt = clean(el.getAttribute('alt') || '');
      const r = role(el);
      if (r) return [{line: interactiveLine(el, r), kids: []}];
      return alt ? [{line: 'img ' + q(cut(alt, MAX_ALT)), kids: []}] : [];
    }
    const r = role(el);
    if (r) return [{line: interactiveLine(el, r), kids: []}];

    const kids = [];
    if (el.shadowRoot) for (const c of el.shadowRoot.childNodes) visitInto(kids, c);
    for (const c of el.childNodes) visitInto(kids, c);

    const aria = (el.getAttribute('role') || '').toLowerCase();
    const h = /^H([1-6])$/.exec(tag);
    if (h || aria === 'heading') {
      const level = h ? h[1] : (el.getAttribute('aria-level') || '2');
      return [titled('heading ' + level, kids)];
    }
    if (tag === 'UL' || tag === 'OL' || aria === 'list') return [node('list', kids)];
    if (tag === 'LI' || aria === 'listitem') return [titled('listitem', kids)];
    if (tag === 'TABLE' || aria === 'table' || aria === 'grid') return [node('table', kids)];
    if (tag === 'TR' || aria === 'row') return [node('row', kids)];
    if (tag === 'TD' || tag === 'TH' || aria === 'cell' || aria === 'gridcell' || aria === 'columnheader' || aria === 'rowheader') return [titled('cell', kids)];
    if (tag === 'DIALOG' || aria === 'dialog' || aria === 'alertdialog') {
      const n = name(el, '');
      return [node('dialog' + (el.getAttribute('aria-label') || el.getAttribute('aria-labelledby') ? ' ' + q(n) : ''), kids)];
    }
    const landmark = LANDMARK_ROLES.has(aria) ? aria : LANDMARKS[tag];
    if (landmark) {
      const lb = el.getAttribute('aria-labelledby');
      const n = lb ? byIds(lb) : clean(el.getAttribute('aria-label') || '');
      if (n) return [node('region ' + q(cut(n, MAX_NAME)), kids)];
    }
    // A wrapper adds no line: its content joins the parent's, kept
    // apart from neighbouring text when it is a block.
    const display = style(el).display;
    if (INLINE.has(display) && tag !== 'BR') return kids;
    return [BREAK, ...kids, BREAK];
  };

  const visitInto = (out, n) => {
    if (n.nodeType === 3) {
      if (n.nodeValue.trim() === '') { if (n.nodeValue.length) pushText(out, ' '); return; }
      if (textShown(n)) pushText(out, n.nodeValue);
      return;
    }
    if (n.nodeType !== 1) return;
    append(out, visit(n));
  };

  const root = [];
  if (document.body) visitInto(root, document.body);
  const tree = finish(root);

  const lines = [];
  const emit = (items, depth) => {
    for (const it of items) {
      if (lines.length >= MAX_LINES) return;
      lines.push([depth, it.line]);
      emit(it.kids, depth + 1);
    }
  };
  emit(tree, 0);

  window.__kivaliRefs = refs;
  return {
    title: document.title,
    url: location.href,
    width: window.innerWidth,
    height: window.innerHeight,
    scrollY: Math.round(window.scrollY),
    docHeight: Math.max(docEl.scrollHeight, document.body ? document.body.scrollHeight : 0),
    lines,
    refs: refInfo,
  };
})()
