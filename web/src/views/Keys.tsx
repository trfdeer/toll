import { Add, Close, Edit, Pause, Play, Renew, TrashCan } from "@carbon/icons-react";
import {
  Button,
  Checkbox,
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
import type { TableColumn } from "react-data-table-component";
import { useCallback, useEffect, useState } from "react";
import StatusTag, { type Status } from "../components/StatusTag";
import Table from "../components/Table";
import {
  createKey,
  deleteKey,
  getKeys,
  getProfiles,
  revokeKey,
  rotateKey,
  updateKey,
} from "../lib/api";
import { errorMessage } from "../lib/errors";
import type { Profile } from "../lib/types";
import {
  serverTableProps,
  useServerRows,
  type ServerTableQuery,
} from "../lib/useServerRows";
import type { VirtualKey } from "../gen/toll/admin/v1/keys_pb";

// KeyRow adds the stable table key; keys are addressed by name.
type KeyRow = VirtualKey & { id: string };

const STATUS_VALUES = ["active", "paused", "revoked"];

// A one-time secret banner: create and rotate both reveal a plaintext once.
interface Flash {
  title: string;
  plaintext: string;
}

export default function Keys() {
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [flash, setFlash] = useState<Flash | null>(null);
  const [open, setOpen] = useState(false);
  // editing is the key being edited, or null when the modal creates one.
  const [editing, setEditing] = useState<VirtualKey | null>(null);
  const [name, setName] = useState("");
  const [profile, setProfile] = useState("All");
  const [pausedDraft, setPausedDraft] = useState(false);
  const [busy, setBusy] = useState(false);

  // The profile dropdown needs the full list, not one page.
  const loadProfiles = useCallback(() => {
    getProfiles({ limit: 0 })
      .then((res) => setProfiles(res.profiles))
      .catch((e: unknown) => setError(errorMessage(e)));
  }, []);

  useEffect(loadProfiles, [loadProfiles]);

  const fetchKeys = useCallback(
    (q: ServerTableQuery) =>
      getKeys(q).then((r) => ({
        rows: r.keys.map((k) => ({ ...k, id: k.name })),
        total: r.total,
      })),
    [],
  );
  const table = useServerRows<KeyRow>(fetchKeys, {
    onError: (e) => setError(errorMessage(e)),
  });

  const resetForm = () => {
    setName("");
    setProfile("All");
    setPausedDraft(false);
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
    setPausedDraft(k.paused);
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
        // PATCH semantics: only the fields that changed ride along; the
        // profile is always sent (an empty selection would reset to All).
        await updateKey(editing.name, {
          newName: name.trim() !== editing.name ? name.trim() : undefined,
          profile,
          paused: pausedDraft,
        });
        table.reload();
        setOpen(false);
        setEditing(null);
        resetForm();
      } else {
        const res = await createKey(name.trim(), profile);
        setFlash({ title: "Key created", plaintext: res.plaintext });
        table.reload();
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
      table.reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  // setPaused replaces the pause/resume route pair with a UpdateKey patch.
  const setPaused = async (keyName: string, paused: boolean) => {
    setError(null);
    try {
      await updateKey(keyName, { paused });
      table.reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  // rotate replaces a key's secret under the same name and profile.
  const rotate = async (keyName: string) => {
    setError(null);
    try {
      const res = await rotateKey(keyName);
      setFlash({ title: "Key rotated", plaintext: res.plaintext });
      table.reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const remove = async (keyName: string) => {
    setError(null);
    try {
      await deleteKey(keyName);
      table.reload();
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
          onClick={() => setPaused(k.name, !paused)}
        >
          {paused ? <Play /> : <Pause />}
        </IconButton>
        <IconButton
          kind="ghost"
          size="sm"
          label="Rotate secret"
          disabled={revoked}
          onClick={() => rotate(k.name)}
        >
          <Renew />
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

  const columns: TableColumn<KeyRow>[] = [
    {
      id: "name",
      name: "Name",
      selector: (k) => k.name,
      sortable: true,
      filterable: true,
    },
    {
      id: "profile",
      name: "Profile",
      selector: (k) => k.profile,
      sortable: true,
      filterable: true,
    },
    {
      id: "status",
      name: "Status",
      selector: (k) => status(k),
      // Status is derived from the revoked/paused columns, which the schema
      // filters on individually; the API layer translates the set filter.
      sortable: false,
      filterable: true,
      filterType: "set",
      filterOptions: { values: STATUS_VALUES },
      cell: (k) => <StatusTag status={status(k)} />,
    },
    {
      id: "actions",
      name: "",
      right: true,
      width: "220px",
      cell: (k) => <div className="row-actions">{actionsFor(k)}</div>,
    },
  ];

  return (
    <Grid>
      <Column lg={{ span: 13, offset: 3 }}>
        <Stack gap={5}>
          {flash && (
            <InlineNotification
              kind="success"
              lowContrast
              title={flash.title}
              subtitle={`Copy it now, it will not be shown again: ${flash.plaintext}`}
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

          <div className="table-block">
            <div className="table-toolbar">
              <Button renderIcon={Add} onClick={openCreate}>
                Create key
              </Button>
            </div>
            <Table
              columns={columns}
              data={table.rows}
              noDataComponent="No keys yet."
              persistTableHead
              storageKey="keys"
              {...serverTableProps(table)}
            />
          </div>
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
          {editing && (
            <Checkbox
              id="key-paused"
              labelText="Paused"
              checked={pausedDraft}
              onChange={(_, { checked }) => setPausedDraft(checked)}
            />
          )}
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
