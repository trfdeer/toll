import { Add, Pause, Play, TrashCan } from "@carbon/icons-react";
import {
  Button,
  Column,
  Grid,
  IconButton,
  InlineNotification,
  Modal,
  Stack,
  TextInput,
} from "@carbon/react";
import { useCallback, useEffect, useState } from "react";
import PageState from "../components/PageState";
import StatusTag, { type Status } from "../components/StatusTag";
import StructuredTable from "../components/StructuredTable";
import {
  addProvider,
  deleteProvider,
  disableProvider,
  enableProvider,
  getProviders,
} from "../lib/api";
import { errorMessage } from "../lib/errors";
import type { Provider } from "../lib/types";

// status maps a provider onto a badge: disabled by an operator, or unreachable
// on the last discovery sync (its models are hidden either way).
function status(p: Provider): Status {
  if (p.disabled) return "disabled";
  return p.reachable ? "active" : "unreachable";
}

export default function Providers() {
  const [providers, setProviders] = useState<Provider[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [name, setName] = useState("");
  const [baseURL, setBaseURL] = useState("");
  const [apiKey, setApiKey] = useState("");

  const reload = useCallback(() => {
    getProviders()
      .then(setProviders)
      .catch((e: unknown) => setError(errorMessage(e)));
  }, []);

  useEffect(reload, [reload]);

  const resetForm = () => {
    setName("");
    setBaseURL("");
    setApiKey("");
  };

  const openModal = () => {
    setFormError(null);
    setOpen(true);
  };

  const closeModal = () => {
    if (busy) return;
    setOpen(false);
    resetForm();
  };

  const submit = async () => {
    setBusy(true);
    setFormError(null);
    try {
      const res = await addProvider({
        name: name.trim(),
        baseURL: baseURL.trim(),
        apiKey,
      });
      setFlash(
        res.warning
          ? res.warning
          : `Added ${res.name} with ${res.modelCount} model(s).`,
      );
      reload();
      setOpen(false);
      resetForm();
    } catch (err) {
      setFormError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (providerName: string) => {
    setError(null);
    try {
      await deleteProvider(providerName);
      reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const toggle = async (p: Provider) => {
    setError(null);
    try {
      await (p.disabled ? enableProvider(p.name) : disableProvider(p.name));
      reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  if (error && providers === null) return <PageState error={error} />;
  if (providers === null) return <PageState />;

  return (
    <Grid>
      <Column lg={{ span: 13, offset: 3 }}>
        <Stack gap={5}>
          {flash && (
            <InlineNotification
              kind="success"
              lowContrast
              title="Provider added"
              subtitle={flash}
              onCloseButtonClick={() => setFlash(null)}
            />
          )}
          {error && (
            <InlineNotification
              kind="error"
              lowContrast
              title="Something went wrong"
              subtitle={error}
              onCloseButtonClick={() => setError(null)}
            />
          )}

          <StructuredTable
            headers={["Name", "Base URL", "Models in registry", "Status", ""]}
            searchable={false}
            className="providers-table"
            actions={
              <Button renderIcon={Add} onClick={openModal}>
                Add provider
              </Button>
            }
            empty="No providers configured."
            rows={providers.map((p) => [
              p.name,
              p.baseURL,
              p.modelCount,
              <span title={p.lastError || undefined}>
                <StatusTag status={status(p)} />
              </span>,
              <>
                {/* Always clickable: enabling a disabled provider is allowed
                    and reachability is evaluated separately (the badge shows
                    unreachable until the next successful sync). */}
                <IconButton
                  kind="ghost"
                  size="sm"
                  label={p.disabled ? `Enable ${p.name}` : `Disable ${p.name}`}
                  onClick={() => toggle(p)}
                >
                  {p.disabled ? <Play /> : <Pause />}
                </IconButton>
                <IconButton
                  kind="ghost"
                  size="sm"
                  label={`Remove ${p.name}`}
                  onClick={() => remove(p.name)}
                >
                  <TrashCan />
                </IconButton>
              </>,
            ])}
          />
        </Stack>
      </Column>

      <Modal
        open={open}
        modalHeading="Add provider"
        primaryButtonText="Add"
        secondaryButtonText="Cancel"
        primaryButtonDisabled={
          busy || !name.trim() || !baseURL.trim() || !apiKey
        }
        onRequestSubmit={submit}
        onRequestClose={closeModal}
        onSecondarySubmit={closeModal}
      >
        <Stack gap={5}>
          <TextInput
            id="provider-name"
            size="sm"
            labelText="Name"
            placeholder="hyper"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <TextInput
            id="provider-url"
            size="sm"
            labelText="Base URL"
            placeholder="https://hyper.charm.land/v1"
            value={baseURL}
            onChange={(e) => setBaseURL(e.target.value)}
          />
          <TextInput
            id="provider-key"
            size="sm"
            type="password"
            labelText="API key"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
          />
          <p>
            The provider&apos;s model catalog is fetched on add. The key is
            stored with the registry and is never included in an exported
            config.
          </p>
          {formError && (
            <InlineNotification
              kind="error"
              lowContrast
              title="Could not add provider"
              subtitle={formError}
              onCloseButtonClick={() => setFormError(null)}
            />
          )}
        </Stack>
      </Modal>
    </Grid>
  );
}
