(() => {
  const w = window;
  w.__aluxCursorOn = true;
  if (w.__aluxCursor) return "kept";
  w.__aluxCursor = true;
  const ID = "__alux_cursor";
  const MARK = "255,92,170";
  let at = null;
  const reset = "position:fixed;inset:auto;left:0;top:0;margin:0;padding:0;border:0;background:transparent;overflow:visible;pointer-events:none;z-index:2147483647;";
  const raise = (el) => {
    if (!el.showPopover) return;
    try {
      if (el.matches(":popover-open")) el.hidePopover();
      el.showPopover();
    } catch (_) {}
  };
  const cursor = () => {
    let el = document.getElementById(ID);
    if (el) return el;
    if (!document.documentElement) return null;
    at = null;
    el = document.createElement("div");
    el.id = ID;
    el.setAttribute("aria-hidden", "true");
    el.setAttribute("popover", "manual");
    el.style.cssText = reset + "width:28px;height:28px;";
    el.innerHTML = '<svg width="28" height="28" viewBox="0 0 24 24"><path d="M3 2 L3 19 L7.5 15 L10.5 22 L13.5 20.8 L10.6 14 L17 14 Z" fill="#111" stroke="#fff" stroke-width="1.5" stroke-linejoin="round"/></svg>';
    document.documentElement.appendChild(el);
    return el;
  };
  addEventListener("mousemove", (e) => {
    if (!w.__aluxCursorOn) return;
    const el = cursor();
    if (!el) return;
    el.style.transition = at ? "transform .3s ease-out" : "none";
    el.style.transform = "translate(" + (e.clientX - 3) + "px," + (e.clientY - 2) + "px)";
    if (!at) el.getBoundingClientRect();
    at = [e.clientX, e.clientY];
    raise(el);
  }, true);
  addEventListener("mousedown", (e) => {
    if (!w.__aluxCursorOn || !document.documentElement) return;
    const ring = document.createElement("div");
    ring.setAttribute("aria-hidden", "true");
    ring.setAttribute("popover", "manual");
    const to = "translate(" + (e.clientX - 18) + "px," + (e.clientY - 18) + "px)";
    ring.style.cssText = reset + "width:36px;height:36px;border-radius:50%;border:3px solid rgb(" + MARK + ");box-sizing:border-box;" +
      "transform:" + to + " scale(.4);opacity:1;transition:transform .5s ease-out,opacity .5s ease-out;";
    document.documentElement.appendChild(ring);
    raise(ring);
    const el = document.getElementById(ID);
    if (el) raise(el);
    ring.getBoundingClientRect();
    ring.style.transform = to + " scale(1.3)";
    ring.style.opacity = "0";
    setTimeout(() => ring.remove(), 700);
  }, true);
  return "fresh";
})();
