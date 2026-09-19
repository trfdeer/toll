import { createTheme } from "react-data-table-component";
import type { Theme, TableStyles } from "react-data-table-component";
import { useTheme } from "./theme";

// tollTableTheme maps react-data-table-component's theme tokens onto Carbon
// design tokens, so the tables pick up the app's light/dark theme through the
// CSS variables the Carbon Theme wrapper sets on <html>. No dark-mode override
// block is needed: the variables resolve differently per theme.
export const tollTableTheme = createTheme("toll", {
  primary: "var(--cds-link-primary)",
  text: {
    primary: "var(--cds-text-primary)",
    secondary: "var(--cds-text-secondary)",
    disabled: "var(--cds-text-disabled)",
  },
  background: {
    default: "var(--cds-layer-01)",
  },
  divider: {
    default: "var(--cds-border-subtle-01)",
  },
  button: {
    default: "var(--cds-icon-primary)",
    hover: "var(--cds-layer-hover-01)",
    focus: "var(--cds-focus)",
    disabled: "var(--cds-icon-disabled)",
  },
  selected: {
    default: "var(--cds-layer-selected-01)",
    text: "var(--cds-text-primary)",
  },
  highlightOnHover: {
    default: "var(--cds-layer-hover-01)",
    text: "var(--cds-text-primary)",
  },
} satisfies Partial<Theme>);

// tollTableStyles applies Carbon surfaces (layer colors and borders) to the
// table chrome. The theme above covers text and interaction colors; these are
// inline overrides for the structural surfaces the theme tokens do not reach.
export const tollTableStyles: TableStyles = {
  // The header bar holds the table's action button. Sitting it on the same
  // layer as the body keeps it from reading as an empty grey strip.
  header: {
    style: {
      backgroundColor: "var(--cds-layer-01)",
      borderBottom: "1px solid var(--cds-border-subtle-01)",
      minHeight: "3rem",
    },
  },
  headRow: {
    style: {
      backgroundColor: "var(--cds-layer-accent-01)",
      borderBottomColor: "var(--cds-border-strong-01)",
    },
  },
  headCells: {
    style: {
      color: "var(--cds-text-primary)",
      fontSize: "0.75rem",
      fontWeight: 600,
      letterSpacing: "0.02em",
    },
  },
  cells: {
    style: {
      color: "var(--cds-text-primary)",
      fontSize: "0.8125rem",
    },
  },
  rows: {
    style: {
      backgroundColor: "var(--cds-layer-01)",
      borderBottomColor: "var(--cds-border-subtle-01)",
      color: "var(--cds-text-primary)",
    },
    highlightOnHoverStyle: {
      backgroundColor: "var(--cds-layer-hover-01)",
      borderBottomColor: "var(--cds-border-subtle-01)",
    },
    selectedHighlightStyle: {
      backgroundColor: "var(--cds-layer-selected-01)",
      borderBottomColor: "var(--cds-border-subtle-01)",
    },
  },
  pagination: {
    style: {
      backgroundColor: "var(--cds-layer-01)",
      borderTopColor: "var(--cds-border-subtle-01)",
      color: "var(--cds-text-primary)",
    },
  },
  footer: {
    style: {
      backgroundColor: "var(--cds-layer-accent-01)",
      borderTopColor: "var(--cds-border-strong-01)",
      color: "var(--cds-text-primary)",
    },
  },
  footerCells: {
    style: {
      fontWeight: 600,
      color: "var(--cds-text-primary)",
    },
  },
  noData: {
    style: {
      color: "var(--cds-text-secondary)",
      padding: "2rem 1rem",
      textAlign: "center",
    },
  },
};

// useTableColorMode reports the resolved light/dark mode for the table's
// colorMode prop, which controls native control rendering (checkboxes, the
// rows-per-page select, scrollbars). React Data Table's own "system" mode looks
// for a "dark" class or a "theme" localStorage key that this app does not use,
// so the mode is passed explicitly.
export function useTableColorMode(): "light" | "dark" {
  const { resolved } = useTheme();
  return resolved === "theme-dark" ? "dark" : "light";
}