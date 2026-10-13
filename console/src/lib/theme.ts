export type Theme = "light" | "dark";

const KEY = "john-theme";

/** Reads whatever the pre-paint script in index.html already resolved. */
export function currentTheme(): Theme {
  const set = document.documentElement.getAttribute("data-theme");
  if (set === "dark" || set === "light") return set;
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function setTheme(theme: Theme): Theme {
  document.documentElement.setAttribute("data-theme", theme);
  try {
    localStorage.setItem(KEY, theme);
  } catch {
    // Storage disabled: the choice simply won't survive a reload.
  }
  return theme;
}

export const toggleTheme = () => setTheme(currentTheme() === "dark" ? "light" : "dark");
