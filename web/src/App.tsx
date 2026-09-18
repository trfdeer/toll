import { Download, Settings as SettingsIcon } from "@carbon/icons-react";
import {
  Button,
  Content,
  Header,
  HeaderGlobalAction,
  HeaderGlobalBar,
  HeaderMenuButton,
  InlineLoading,
  Modal,
  SideNav,
  SideNavItems,
  SideNavLink,
  Theme,
  Toggle,
} from "@carbon/react";
import { useEffect, useState } from "react";
import { Link, Route, Routes, useLocation } from "react-router";
import { exportConfig, getSettings, updateSettings } from "./lib/api";
import { errorMessage } from "./lib/errors";
import TollMark from "./components/TollMark";
import { THEME_ICONS, THEME_LABELS, THEME_ORDER, useTheme } from "./lib/theme";
import type { Settings } from "./lib/types";
import Keys from "./views/Keys";
import Models from "./views/Models";
import Profiles from "./views/Profiles";
import Providers from "./views/Providers";
import Usage from "./views/Usage";

// Side-nav entries. `end` marks the index route so it is only highlighted on
// exactly "/" rather than every path.
const NAV = [
  { to: "/", label: "Usage", end: true },
  { to: "/providers", label: "Providers" },
  { to: "/models", label: "Models" },
  { to: "/profiles", label: "Profiles" },
  { to: "/keys", label: "Virtual Keys" },
];

export default function App() {
  const { pref, set: setThemePref, resolved: themeClass } = useTheme();
  const ThemeIcon = THEME_ICONS[pref];
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState<string | null>(null);
  // Runtime settings, loaded once and updated by the Settings modal.
  const [settings, setSettings] = useState<Settings | null>(null);
  const [settingsError, setSettingsError] = useState<string | null>(null);
  const [savingSettings, setSavingSettings] = useState(false);
  const { pathname } = useLocation();

  // Keep the browser tab title in step with the active route. The catch-all
  // route renders Usage, so unknown paths title as Usage too.
  const pageLabel =
    NAV.find((entry) => entry.to === pathname)?.label ?? "Usage";
  useEffect(() => {
    document.title = `${pageLabel} · toll`;
  }, [pageLabel]);

  useEffect(() => {
    getSettings()
      .then(setSettings)
      .catch((e: unknown) => setSettingsError(errorMessage(e)));
  }, []);

  // toggleStorePrompts persists the prompt-storage setting.
  const toggleStorePrompts = async (storePrompts: boolean) => {
    setSavingSettings(true);
    setSettingsError(null);
    try {
      setSettings(await updateSettings({ storePrompts }));
    } catch (e) {
      setSettingsError(errorMessage(e));
    } finally {
      setSavingSettings(false);
    }
  };

  // onExport downloads the current registry as a toll.yaml file.
  const onExport = async () => {
    setExporting(true);
    setExportError(null);
    try {
      const yaml = await exportConfig();
      const url = URL.createObjectURL(
        new Blob([yaml], { type: "application/yaml" }),
      );
      const link = document.createElement("a");
      link.href = url;
      link.download = "toll.yaml";
      link.click();
      URL.revokeObjectURL(url);
    } catch (e) {
      setExportError(errorMessage(e));
    } finally {
      setExporting(false);
    }
  };

  const cycleTheme = () => {
    const next =
      THEME_ORDER[(THEME_ORDER.indexOf(pref) + 1) % THEME_ORDER.length] ??
      "auto";
    setThemePref(next);
  };

  return (
    <Theme className={themeClass}>
      <Header aria-label="toll admin">
        <HeaderMenuButton aria-label="Open menu" onClick={() => {}} />
        <Link className="cds--header__name" to="/">
          <TollMark />
          toll
        </Link>
        <HeaderGlobalBar>
          <HeaderGlobalAction
            aria-label={`Theme: ${THEME_LABELS[pref]}`}
            tooltipAlignment="end"
            onClick={cycleTheme}
          >
            <ThemeIcon size={20} />
          </HeaderGlobalAction>
          <HeaderGlobalAction
            aria-label="Settings"
            tooltipAlignment="end"
            onClick={() => setSettingsOpen(true)}
          >
            <SettingsIcon size={20} />
          </HeaderGlobalAction>
        </HeaderGlobalBar>
        <SideNav aria-label="Side navigation" isPersistent>
          <SideNavItems>
            {NAV.map(({ to, label, end }) => (
              <SideNavLink
                key={to}
                as={Link}
                to={to}
                isActive={end ? pathname === to : pathname.startsWith(to)}
              >
                {label}
              </SideNavLink>
            ))}
          </SideNavItems>
        </SideNav>
      </Header>
      <Content>
        <Routes>
          <Route path="/" element={<Usage />} />
          <Route path="/providers" element={<Providers />} />
          <Route path="/models" element={<Models />} />
          <Route path="/profiles" element={<Profiles />} />
          <Route path="/keys" element={<Keys />} />
          {/* Unknown paths fall back to the Usage view. */}
          <Route path="*" element={<Usage />} />
        </Routes>
      </Content>
      <Modal
        open={settingsOpen}
        modalHeading="Settings"
        passiveModal
        onRequestClose={() => setSettingsOpen(false)}
      >
        <div className="settings-section">
          <Toggle
            id="setting-store-prompts"
            labelText="Store prompt content"
            labelA="Off"
            labelB="On"
            toggled={settings?.storePrompts ?? true}
            disabled={settings === null || savingSettings}
            onToggle={toggleStorePrompts}
          />
          <p className="settings-section__hint">
            When on, request prompts and model responses are stored in a
            separate content database (content.db) so they can be inspected in
            usage. Turn it off to keep only metadata — tokens, cost and timing.
          </p>
          {savingSettings && <InlineLoading description="Saving…" />}
          {settingsError && (
            <p className="settings-section__error">{settingsError}</p>
          )}
        </div>
        <div className="settings-section">
          <p className="settings-section__hint">
            Download the current registry (upstreams and models) as a toll.yaml
            config file. Upstream API keys are never included — each is redacted
            to an <code>api_key_env</code> reference.
          </p>
          {exportError && (
            <p className="settings-section__error">{exportError}</p>
          )}
          <Button renderIcon={Download} onClick={onExport} disabled={exporting}>
            {exporting ? "Exporting…" : "Export config"}
          </Button>
        </div>
      </Modal>
    </Theme>
  );
}
