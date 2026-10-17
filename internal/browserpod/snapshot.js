// The page as a web subagent reads it. Evaluated in the pod's isolated
// world; returns {title, url, viewport, lines, refs}. Each line is
// [depth, text, top]: top is how far down the document the line's
// element starts, so a snapshot over its limit can keep the lines at
// the current scroll position. Interactive elements carry a ref "eN",
// and the element is kept in window.__kivaliRefs[N] for the commands
// that act on it.
(() => {
  const MAX_TEXT = 300, MAX_NAME = 120, MAX_ALT = 80, MAX_VALUE = 100, MAX_LINES = 20000;
  const SKIP = new Set(['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE', 'HEAD', 'META', 'LINK', 'OPTION', 'OPTGROUP', 'DATALIST', 'AREA', 'MAP', 'SLOT']);
  const ROLES = new Set(['button', 'link', 'tab', 'menuitem', 'option', 'checkbox', 'switch', 'textbox', 'combobox', 'radio', 'menuitemcheckbox', 'menuitemradio', 'searchbox', 'slider', 'spinbutton', 'treeitem']);
  const LANDMARKS = {NAV: 'navigation', MAIN: 'main', HEADER: 'banner', FOOTER: 'contentinfo', ASIDE: 'complementary', SECTION: 'region', FORM: 'form', SEARCH: 'search'};
  const LANDMARK_ROLES = new Set(['region', 'navigation', 'main', 'banner', 'contentinfo', 'complementary', 'form', 'search']);
  const INLINE = new Set(['inline', 'contents']);
  // Descendants that get a ref of their own inside a control that
  // holds them.
  const INTERACTIVE = 'a[href], button, input, select, textarea, summary, [contenteditable], [onclick], [tabindex]:not([tabindex^="-"]), ' +
    Array.from(ROLES).map(r => '[role="' + r + '"]').join(', ');
  // Ways of hiding that leave a form control in place, where its label
  // stands for it and a click or keystroke still reaches it.
  const IN_PLACE = new Set(['opacity', 'sr', 'size']);
  const FORM_TAGS = new Set(['INPUT', 'SELECT', 'TEXTAREA']);

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
  const rects = new Map();
  const rect = el => {
    let r = rects.get(el);
    if (!r) { r = el.getBoundingClientRect(); rects.set(el, r); }
    return r;
  };

  const transparent = c => !c || c === 'transparent' || /^rgba\(.*,\s*0\)$/.test(c);

  // The background a text node is drawn on: the nearest ancestor with
  // an opaque background colour, white when none has one. An image or
  // gradient on the way is 'image', a background no colour equals.
  const bgCache = new Map();
  const background = el => {
    if (!el || el.nodeType !== 1) return 'rgb(255, 255, 255)';
    if (bgCache.has(el)) return bgCache.get(el);
    const cs = style(el);
    let v;
    if (cs.backgroundImage !== 'none') {
      v = 'image';
    } else {
      const bg = cs.backgroundColor;
      v = transparent(bg) ? background(el.parentElement) : bg;
    }
    bgCache.set(el, v);
    return v;
  };

  const offDocument = r => r.right + sx < 0 || r.bottom + sy < 0 || r.left + sx > docWidth;

  const srOnly = (cs, r) => {
    const clipped = cs.clipPath === 'inset(100%)' || cs.clipPath === 'inset(50%)' ||
      /^rect\(0(px)?,? 0(px)?,? 0(px)?,? 0(px)?\)$/.test(cs.clip);
    return clipped && r.width <= 1 && r.height <= 1;
  };

  // Clipping by overflow: hidden ancestors. A clip is the box content
  // must touch to show; null clips nothing. Positioned elements escape
  // the clips of ancestors below their containing block, so the walk
  // carries two: one for content in flow, one for absolutely positioned
  // content.
  const NO_CLIP = {clip: null, posClip: null};
  const intersect = (a, b) => !a ? b : !b ? a : {
    left: Math.max(a.left, b.left), top: Math.max(a.top, b.top),
    right: Math.min(a.right, b.right), bottom: Math.min(a.bottom, b.bottom),
  };
  const outside = (r, clip) => r.right <= clip.left || r.left >= clip.right || r.bottom <= clip.top || r.top >= clip.bottom;
  // ownClip is the box an element clips its content to, or null. The
  // root elements' overflow applies to the viewport, not their boxes.
  const ownClip = (el, cs) => {
    if (el.tagName === 'BODY' || el.tagName === 'HTML') return null;
    const cx = cs.overflowX === 'hidden' || cs.overflowX === 'clip';
    const cy = cs.overflowY === 'hidden' || cs.overflowY === 'clip';
    if (!cx && !cy) return null;
    const r = rect(el);
    if (r.width === 0 || r.height === 0) return null;
    return {left: cx ? r.left : -Infinity, right: cx ? r.right : Infinity, top: cy ? r.top : -Infinity, bottom: cy ? r.bottom : Infinity};
  };
  // appliedClip is the clip an element's own box is judged against.
  const appliedClip = (cs, st) => cs.position === 'absolute' ? st.posClip : cs.position === 'fixed' ? null : st.clip;
  // nextClips is what the element's children are judged against.
  const nextClips = (el, cs, applied, st) => {
    const own = ownClip(el, cs);
    if (cs.position !== 'static') {
      const c = intersect(applied, own);
      return {clip: c, posClip: c};
    }
    return own ? {clip: intersect(st.clip, own), posClip: st.posClip} : st;
  };

  // hidden says why an element is not shown, or '' when it is:
  // 'attr' (hidden or aria-hidden), 'display', 'visibility' (children
  // that set visibility back may still show), 'opacity', 'sr' (a
  // screen-reader-only clip), 'size' (a zero-size box that clips),
  // 'off' (off the document) or 'clip' (wholly outside an ancestor's
  // overflow: hidden box, given as clip).
  const hidden = (el, clip) => {
    if (el.hidden || el.getAttribute('aria-hidden') === 'true') return 'attr';
    const cs = style(el);
    if (cs.display === 'none') return 'display';
    if (cs.visibility === 'hidden' || cs.visibility === 'collapse') return 'visibility';
    if (parseFloat(cs.opacity) === 0) return 'opacity';
    if (cs.display === 'contents') return '';
    const r = rect(el);
    if (srOnly(cs, r)) return 'sr';
    if (r.width === 0 || r.height === 0) {
      // A zero-size box clips what it holds when it hides overflow, and
      // a form control has nothing else to show; otherwise its children
      // may still show (inline containers).
      if (FORM_TAGS.has(el.tagName) || cs.overflow !== 'visible' || cs.overflowX !== 'visible' || cs.overflowY !== 'visible') return 'size';
    } else if (offDocument(r)) {
      return 'off';
    } else if (clip && cs.position !== 'sticky' && outside(r, clip)) {
      return 'clip';
    }
    return '';
  };

  // textRect is the box of a text node that can be seen: one in the
  // document, with a font size, visible, and a colour unlike its
  // background. null when it cannot.
  const textRect = node => {
    const el = node.parentElement;
    if (!el) return null;
    const cs = style(el);
    if (cs.visibility === 'hidden' || cs.visibility === 'collapse') return null;
    if (parseFloat(cs.fontSize) === 0) return null;
    if (transparent(cs.color)) return null;
    if (cs.color === background(el)) return null;
    const range = document.createRange();
    range.selectNodeContents(node);
    const r = range.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) return null;
    if (offDocument(r)) return null;
    return r;
  };

  const textOf = el => {
    const parts = [];
    const walk = n => {
      if (n.nodeType === 3) { if (textRect(n)) parts.push(n.nodeValue); return; }
      if (n.nodeType !== 1 || SKIP.has(n.tagName)) return;
      const h = hidden(n, null);
      if (h && h !== 'visibility') return;
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

  // ariaName is the name an element is given outright, '' when none.
  const ariaName = el => {
    const lb = el.getAttribute('aria-labelledby');
    if (lb) { const n = byIds(lb); if (n) return cut(n, MAX_NAME); }
    const al = el.getAttribute('aria-label');
    if (al && clean(al)) return cut(clean(al), MAX_NAME);
    return '';
  };

  // labelled reports a form control with a label or an aria name.
  const labelled = el => (el.labels && el.labels.length > 0) || el.hasAttribute('aria-label') || el.hasAttribute('aria-labelledby');

  const name = (el, r) => {
    const an = ariaName(el);
    if (an) return an;
    const tag = el.tagName;
    if (FORM_TAGS.has(tag)) {
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

  // newRef gives an element to act on its ref, before its content is
  // walked so that refs read in page order.
  const newRef = (el, r) => {
    refs.push(el);
    refInfo.push({role: r, name: ''});
    return refs.length - 1;
  };

  // interactiveLine describes the element with ref idx: n is the name
  // on its line, infoName (n when omitted) the one the commands report.
  const interactiveLine = (idx, el, r, n, infoName) => {
    refInfo[idx].name = infoName === undefined ? n : infoName;
    let line = '[e' + idx + '] ' + r + (n ? ' ' + q(n) : '');
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
      const accept = clean(el.getAttribute('accept') || '');
      if (accept) line += ' accept=' + q(cut(accept, MAX_VALUE));
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

  // topOf is how far down the document an element starts: its own box,
  // or its parent's when it has none.
  const topOf = el => {
    for (let e = el; e && e.nodeType === 1; e = e.parentElement) {
      const r = rect(e);
      if (r.width || r.height) return r.top + sy;
    }
    return sy;
  };

  // An item is {line, kids, top} for an element worth a line, {text,
  // top} for a run of text, or BREAK where a block boundary keeps text
  // runs apart.
  const BREAK = {brk: true};

  const pushText = (out, s, top) => {
    const last = out[out.length - 1];
    if (last && last.text !== undefined) {
      last.text += s;
      if (last.top === undefined) last.top = top;
    } else {
      out.push({text: s, top});
    }
  };

  const append = (out, items) => {
    for (const it of items) {
      if (it.text !== undefined) pushText(out, it.text, it.top);
      else out.push(it);
    }
  };

  const node = (line, kids, top) => ({line, kids: finish(kids), top});

  // finish turns text runs into lines (keeping the text as raw) and
  // drops breaks.
  const finish = items => {
    const out = [];
    for (const it of items) {
      if (it === BREAK) continue;
      if (it.text !== undefined) {
        const t = clean(it.text);
        if (t) out.push({line: 'text ' + q(cut(t, MAX_TEXT)), kids: [], top: it.top, raw: t});
        continue;
      }
      out.push(it);
    }
    return out;
  };

  // loneText is the text of a finished list that is one text line,
  // else null.
  const loneText = f => f.length === 1 && f[0].raw !== undefined && f[0].kids.length === 0 ? f[0].raw : null;

  // titled puts a lone text child on its parent's line.
  const titled = (label, kids, top) => {
    const f = finish(kids);
    const t = loneText(f);
    if (t !== null) return {line: label + ' ' + q(cut(t, MAX_TEXT)), kids: [], top};
    return {line: label, kids: f, top};
  };

  let count = 0;

  const walkChildren = (el, out, st) => {
    if (el.shadowRoot) for (const c of el.shadowRoot.childNodes) visitInto(out, c, st);
    for (const c of el.childNodes) visitInto(out, c, st);
  };

  const visit = (el, st) => {
    if (++count > MAX_LINES * 4) return [];
    const tag = el.tagName;
    if (SKIP.has(tag)) return [];
    const cs = style(el);
    const applied = appliedClip(cs, st);
    const h = hidden(el, applied);
    if (h) {
      if (FORM_TAGS.has(tag)) {
        // A hidden form control may still work: a file input takes
        // web_upload however it is hidden, a labelled checkbox or radio
        // is toggled through its label, and a labelled control hidden
        // in place takes focus and keys.
        const r = role(el);
        if (r && (r === 'file' || (labelled(el) && (r === 'checkbox' || r === 'radio' || IN_PLACE.has(h))))) {
          return [{line: interactiveLine(newRef(el, r), el, r, name(el, r)) + ' hidden', kids: [], top: topOf(el)}];
        }
        return [];
      }
      if (h !== 'visibility') return [];
      // Children that set visibility back to visible show.
      const kids = [];
      walkChildren(el, kids, nextClips(el, cs, applied, st));
      return kids.length ? [BREAK, ...kids, BREAK] : [];
    }
    const top = topOf(el);
    if (tag === 'svg' || tag === 'SVG') {
      const n = svgName(el);
      return n ? [{line: 'img ' + q(cut(n, MAX_ALT)), kids: [], top}] : [];
    }
    if (el.namespaceURI !== 'http://www.w3.org/1999/xhtml' && el.namespaceURI !== null) return [];
    if (tag === 'IFRAME' || tag === 'FRAME') {
      let label = el.getAttribute('title') || '';
      if (!label) { try { label = new URL(el.src, location.href).host; } catch (e) { label = ''; } }
      return [{line: 'iframe ' + q(cut(clean(label), MAX_NAME)), kids: [], top}];
    }
    if (tag === 'IMG') {
      const alt = clean(el.getAttribute('alt') || '');
      const r = role(el);
      if (r) return [{line: interactiveLine(newRef(el, r), el, r, name(el, r)), kids: [], top}];
      return alt ? [{line: 'img ' + q(cut(alt, MAX_ALT)), kids: [], top}] : [];
    }
    const r = role(el);
    const st2 = nextClips(el, cs, applied, st);
    if (r) {
      // A control is one line, named by its content. A clickable
      // wrapper, and a control holding controls, lists its content as
      // kids instead, each with a ref of its own.
      const holds = !FORM_TAGS.has(tag) && !el.isContentEditable && (r === 'clickable' || el.querySelector(INTERACTIVE) !== null);
      if (!holds) return [{line: interactiveLine(newRef(el, r), el, r, name(el, r)), kids: [], top}];
      const idx = newRef(el, r);
      const kids = [];
      walkChildren(el, kids, st2);
      const f = finish(kids);
      let n = ariaName(el);
      const t = loneText(f);
      if (!n && t !== null) { n = cut(t, MAX_NAME); f.length = 0; }
      return [{line: interactiveLine(idx, el, r, n, name(el, r)), kids: f, top}];
    }

    const kids = [];
    walkChildren(el, kids, st2);

    const aria = (el.getAttribute('role') || '').toLowerCase();
    const hd = /^H([1-6])$/.exec(tag);
    if (hd || aria === 'heading') {
      const level = hd ? hd[1] : (el.getAttribute('aria-level') || '2');
      return [titled('heading ' + level, kids, top)];
    }
    if (tag === 'UL' || tag === 'OL' || aria === 'list') return [node('list', kids, top)];
    if (tag === 'LI' || aria === 'listitem') return [titled('listitem', kids, top)];
    if (tag === 'TABLE' || aria === 'table' || aria === 'grid') return [node('table', kids, top)];
    if (tag === 'TR' || aria === 'row') return [node('row', kids, top)];
    if (tag === 'TD' || tag === 'TH' || aria === 'cell' || aria === 'gridcell' || aria === 'columnheader' || aria === 'rowheader') return [titled('cell', kids, top)];
    if (tag === 'DIALOG' || aria === 'dialog' || aria === 'alertdialog') {
      const n = name(el, '');
      return [node('dialog' + (el.getAttribute('aria-label') || el.getAttribute('aria-labelledby') ? ' ' + q(n) : ''), kids, top)];
    }
    const landmark = LANDMARK_ROLES.has(aria) ? aria : LANDMARKS[tag];
    if (landmark) {
      const lb = el.getAttribute('aria-labelledby');
      const n = lb ? byIds(lb) : clean(el.getAttribute('aria-label') || '');
      if (n) return [node('region ' + q(cut(n, MAX_NAME)), kids, top)];
    }
    // A wrapper adds no line: its content joins the parent's, kept
    // apart from neighbouring text when it is a block.
    if (INLINE.has(cs.display) && tag !== 'BR') return kids;
    return [BREAK, ...kids, BREAK];
  };

  const visitInto = (out, n, st) => {
    if (n.nodeType === 3) {
      if (n.nodeValue.trim() === '') { if (n.nodeValue.length) pushText(out, ' ', undefined); return; }
      const r = textRect(n);
      if (r) pushText(out, n.nodeValue, r.top + sy);
      return;
    }
    if (n.nodeType !== 1) return;
    append(out, visit(n, st));
  };

  const root = [];
  if (document.body) visitInto(root, document.body, NO_CLIP);
  const tree = finish(root);

  const lines = [];
  const emit = (items, depth) => {
    for (const it of items) {
      if (lines.length >= MAX_LINES) return;
      lines.push([depth, it.line, Math.round(it.top === undefined ? sy : it.top)]);
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
