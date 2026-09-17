import { Add, Close, Edit, Pause, Play, TrashCan } from "@carbon/icons-react";
import {
  Button,
  Column,
  Grid,
  IconButton,
  InlineLoading,
  InlineNotification,
  Modal,
  Stack,
  TextInput,
} from "@carbon/react";
import { useCallback, useEffect, useState } from "react";
import KeyFilters from "../components/KeyFilters";
import StatusTag, { type Status } from "../components/StatusTag";
import StructuredTable from "../components/StructuredTable";
import {
  createKey,
  deleteKey,
  getKeys,
  getModels,
  pauseKey,
  resumeKey,
  revokeKey,
  updateKey,
} from "../lib/api";
import { errorMessage } from "../lib/errors";
import type { KeyFilter, Model, VirtualKey } from "../lib/types";

const EMPTY: KeyFilter = { mode: "none", values: [] };

// describe renders a filter for the table: "all providers" when unrestricted,
// otherwise the mode and the selected values.
function describe(filter: KeyFilter, noun: string): string {
  if (!filter || filter.mode === "none" || !filter.values?.length)
    return `all ${noun}`;
  if (filter.mode === "include") return `${noun}: ${filter.values.join(", ")}`;
  return `all ${noun} except: ${filter.values.join(", ")}`;
}

export default function Keys() {
  const [keys, setKeys] = useState<VirtualKey[] | null>(null);
  const [models, setModels] = useState<Model[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  // editing is the key being edited, or null when the modal creates one.
  const [editing, setEditing] = useState<VirtualKey | null>(null);
  const [name, setName] = useState("");
  const [provider, setProvider] = useState<KeyFilter>(EMPTY);
  const [model, setModel] = useState<KeyFilter>(EMPTY);
  const [busy, setBusy] = useState(false);

  const reload = useCallback(() => {
    getKeys()
      .then((k) => setKeys(k.keys))
      .catch((e: unknown) => setError(errorMessage(e)));
  }, []);

  useEffect(() => {
    reload();
    getModels()
      .then(setModels)
      .catch((e: unknown) => setError(errorMessage(e)));
  }, [reload]);

  const resetForm = () => {
    setName("");
    setProvider(EMPTY);
    setModel(EMPTY);
  };

  const openCreate = () => {
    setEditing(null);
    resetForm();
    setFormError(null);
    setOpen(true);
  };

  const openEdit = (k: VirtualKey) => {
    setEditing(k);
    setName(k.name);
    setProvider(k.providerFilter ?? EMPTY);
    setModel(k.modelFilter ?? EMPTY);
    setFormError(null);
    setOpen(true);
  };

  const closeModal = () => {
    if (busy) return;
    setOpen(false);
    setEditing(null);
    resetForm();
  };

  const filtersValid = (f: KeyFilter) =>
    f.mode === "none" || f.values.length > 0;

  const submit = async () => {
    setBusy(true);
    setFormError(null);
    try {
      if (editing) {
        await updateKey(editing.name, {
          name: name.trim(),
          providerFilter: provider,
          modelFilter: model,
        });
        reload();
        setOpen(false);
        setEditing(null);
        resetForm();
      } else {
        const res = await createKey(name.trim(), provider, model);
        setFlash(res.plaintext);
        reload();
        setOpen(false);
        resetForm();
      }
    } catch (err) {
      setFormError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (keyName: string) => {
    setError(null);
    try {
      await revokeKey(keyName);
      reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const pause = async (keyName: string) => {
    setError(null);
    try {
      await pauseKey(keyName);
      reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const resume = async (keyName: string) => {
    setError(null);
    try {
      await resumeKey(keyName);
      reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const remove = async (keyName: string) => {
    setError(null);
    try {
      await deleteKey(keyName);
      reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const status = (k: VirtualKey): Status =>
    k.revoked ? "revoked" : k.paused ? "paused" : "active";

  const actionsFor = (k: VirtualKey) => {
    const s = status(k);
    const paused = s === "paused";
    const revoked = s === "revoked";
    return (
      <>
        <IconButton
          kind="ghost"
          size="sm"
          label={`Edit ${k.name}`}
          onClick={() => openEdit(k)}
        >
          <Edit />
        </IconButton>
        <IconButton
          kind="ghost"
          size="sm"
          label={paused ? "Resume" : "Pause"}
          disabled={revoked}
          onClick={() => (paused ? resume(k.name) : pause(k.name))}
        >
          {paused ? <Play /> : <Pause />}
        </IconButton>
        <IconButton
          kind="ghost"
          size="sm"
          label="Revoke"
          disabled={s !== "active"}
          onClick={() => revoke(k.name)}
        >
          <Close />
        </IconButton>
        <IconButton
          kind="ghost"
          size="sm"
          label="Delete"
          disabled={!revoked}
          onClick={() => remove(k.name)}
        >
          <TrashCan />
        </IconButton>
      </>
    );
  };

  if (error && keys === null) return <p>Failed to load: {error}</p>;
  if (keys === null) return <InlineLoading description="Loading…" />;

  // Providers are the upstreams models were discovered from, not a prefix of
// the gateway ID (an alias rule may make the ID carry no provider at all).
  const providers = Array.from(
    new Set(models.map((m) => m.upstream).filter(Boolean)),
  ).sort();
  const modelIds = models.map((m) => m.gatewayId);
  const modelLabel = (id: string) => {
    const m = models.find((x) => x.gatewayId === id);
    return m ? `${m.displayName} (${m.gatewayId})` : id;
  };

  return (
    <Grid>
      <Column lg={{ span: 13, offset: 3 }}>
        <Stack gap={5}>
          {flash && (
            <InlineNotification
              kind="success"
              lowContrast
              title="Key created"
              subtitle={`Copy it now, it will not be shown again: ${flash}`}
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
            headers={["Name", "Providers", "Models", "Status", ""]}
            searchable={false}
            className="keys-table"
            actions={
              <Button renderIcon={Add} onClick={openCreate}>
                Create key
              </Button>
            }
            empty="No keys yet."
            rows={keys.map((k) => {
              const s = status(k);
              return [
                k.name,
                describe(k.providerFilter, "providers"),
                describe(k.modelFilter, "models"),
                <StatusTag status={s} />,
                <div className="key-actions">{actionsFor(k)}</div>,
              ];
            })}
          />
        </Stack>
      </Column>

      <Modal
        open={open}
        className="key-modal"
        modalHeading={editing ? "Edit virtual key" : "New virtual key"}
        primaryButtonText={editing ? "Save" : "Create"}
        secondaryButtonText="Cancel"
        primaryButtonDisabled={
          busy ||
          !name.trim() ||
          !filtersValid(provider) ||
          !filtersValid(model)
        }
        onRequestSubmit={submit}
        onRequestClose={closeModal}
        onSecondarySubmit={closeModal}
      >
        <Stack gap={5}>
          <TextInput
            id="key-name"
            size="sm"
            labelText="Name"
            placeholder="my-app"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <KeyFilters
            id="key-provider"
            title="Provider filter"
            mode={provider.mode}
            values={provider.values}
            items={providers}
            onMode={(mode) => setProvider({ mode, values: [] })}
            onValues={(values) => setProvider((p) => ({ ...p, values }))}
          />
          <KeyFilters
            id="key-model"
            title="Model filter"
            mode={model.mode}
            values={model.values}
            items={modelIds}
            itemToString={modelLabel}
            filterable
            onMode={(mode) => setModel({ mode, values: [] })}
            onValues={(values) => setModel((m) => ({ ...m, values }))}
          />
          <p>
            A model is allowed when its provider passes the provider filter and
            the model itself passes the model filter. No filter allows
            everything.
          </p>
          {formError && (
            <InlineNotification
              kind="error"
              lowContrast
              title={editing ? "Could not update key" : "Could not create key"}
              subtitle={formError}
              onCloseButtonClick={() => setFormError(null)}
            />
          )}
        </Stack>
      </Modal>
    </Grid>
  );
}
