// Light or dark follows the system (prefers-color-scheme), applied as the
// design system's data-theme on <html>. In `vite dev`, ?theme=dark|light
// forces one.

export function initTheme() {
  const html = document.documentElement;
  const params = new URLSearchParams(location.search);
  const forced = import.meta.env.DEV ? params.get("theme") : null;
  if (forced === "dark" || forced === "light") {
    html.dataset.theme = forced;
    return;
  }
  const mq = window.matchMedia("(prefers-color-scheme: dark)");
  const apply = () => {
    html.dataset.theme = mq.matches ? "dark" : "light";
  };
  apply();
  mq.addEventListener("change", apply);
}
