import { Add, Close, Edit, Pause, Play, TrashCan } from "@carbon/icons-react";
import {
  Button,
  Column,
  Grid,
  IconButton,
  InlineNotification,
  Modal,
  Select,
  SelectItem,
  Stack,
  TextInput,
} from "@carbon/react";
import { useCallback, useEffect, useState } from "react";
import PageState from "../components/PageState";
import StatusTag, { type Status } from "../components/StatusTag";
import StructuredTable from "../components/StructuredTable";
import {
  createKey,
  deleteKey,
  getKeys,
  getProfiles,
  pauseKey,
  resumeKey,
  revokeKey,
  updateKey,
} from "../lib/api";
import { errorMessage } from "../lib/errors";
import type { Profile, VirtualKey } from "../lib/types";

export default function Keys() {
  const [keys, setKeys] = useState<VirtualKey[] | null>(null);
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  // editing is the key being edited, or null when the modal creates one.
  const [editing, setEditing] = useState<VirtualKey | null>(null);
  const [name, setName] = useState("");
  const [profile, setProfile] = useState("All");
  const [busy, setBusy] = useState(false);

  const reload = useCallback(() => {
    getKeys()
      .then((k) => setKeys(k.keys))
      .catch((e: unknown) => setError(errorMessage(e)));
    getProfiles()
      .then((res) => setProfiles(res.profiles))
      .catch((e: unknown) => setError(errorMessage(e)));
  }, []);

  useEffect(reload, [reload]);

  const resetForm = () => {
    setName("");
    setProfile("All");
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
    setProfile(k.profile || "All");
    setFormError(null);
    setOpen(true);
  };

  const closeModal = () => {
    if (busy) return;
    setOpen(false);
    setEditing(null);
    resetForm();
  };

  const submit = async () => {
    setBusy(true);
    setFormError(null);
    try {
      if (editing) {
        await updateKey(editing.name, { name: name.trim(), profile });
        reload();
        setOpen(false);
        setEditing(null);
        resetForm();
      } else {
        const res = await createKey(name.trim(), profile);
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

  if (error && keys === null) return <PageState error={error} />;
  if (keys === null) return <PageState />;

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
            headers={["Name", "Profile", "Status", ""]}
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
                k.profile,
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
        primaryButtonDisabled={busy || !name.trim() || !profile}
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
          <Select
            id="key-profile"
            size="sm"
            labelText="Profile"
            value={profile}
            onChange={(e) => setProfile(e.target.value)}
          >
            {profiles.map((p) => (
              <SelectItem
                key={p.name}
                value={p.name}
                text={p.isDefault ? `${p.name} (default)` : p.name}
              />
            ))}
          </Select>
          <p>
            The profile defines which providers and models this key may use. It
            can be shared by multiple keys and is managed on the Profiles page.
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
