import type { CarbonIconType } from "@carbon/icons-react";
import { Contrast, Moon, Sun } from "@carbon/icons-react";
import { useEffect, useState } from "react";

const THEME_KEY = "toll-admin-theme";

// Theme preference, and the Carbon theme class it resolves to.
export type ThemePref = "auto" | "light" | "dark";
export type ResolvedTheme = "theme-light" | "theme-dark";

// Icon and label shown on the header trigger for the active theme preference.
export const THEME_ICONS: Record<ThemePref, CarbonIconType> = {
  auto: Contrast,
  light: Sun,
  dark: Moon,
};
export const THEME_LABELS: Record<ThemePref, string> = {
  auto: "Auto",
  light: "Light",
  dark: "Dark",
};

// Order the header action cycles through when clicked.
export const THEME_ORDER: readonly ThemePref[] = ["auto", "light", "dark"];

function isThemePref(value: unknown): value is ThemePref {
  return (
    typeof value === "string" &&
    (THEME_ORDER as readonly string[]).includes(value)
  );
}

export interface ThemeControls {
  pref: ThemePref;
  set: (pref: ThemePref) => void;
  resolved: ResolvedTheme;
}

// useTheme tracks the theme preference (auto/light/dark). "auto" follows the
// system preference; resolved maps the preference to a Carbon theme name.
export function useTheme(): ThemeControls {
  const [pref, setPref] = useState<ThemePref>(() => {
    const stored = localStorage.getItem(THEME_KEY);
    return isThemePref(stored) ? stored : "auto";
  });
  const [systemDark, setSystemDark] = useState(
    () => window.matchMedia("(prefers-color-scheme: dark)").matches,
  );

  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => setSystemDark(mq.matches);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  const set = (pref: ThemePref) => {
    setPref(pref);
    localStorage.setItem(THEME_KEY, pref);
  };

  const resolved: ResolvedTheme =
    pref === "auto"
      ? systemDark
        ? "theme-dark"
        : "theme-light"
      : pref === "dark"
        ? "theme-dark"
        : "theme-light";

  // Carbon renders portaled surfaces (modals, popovers) on document.body,
  // outside the <Theme> wrapper. Applying the theme class to the root element
  // lets those surfaces inherit the theme's CSS variables too.
  useEffect(() => {
    const root = document.documentElement;
    root.classList.remove("theme-light", "theme-dark");
    root.classList.add(resolved);
  }, [resolved]);

  return { pref, set, resolved };
}
