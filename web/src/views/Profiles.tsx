import { Add, Edit, TrashCan, View } from "@carbon/icons-react";
import {
  Button,
  Column,
  Grid,
  IconButton,
  InlineNotification,
  Modal,
  MultiSelect,
  RadioButton,
  RadioButtonGroup,
  Stack,
  TextInput,
} from "@carbon/react";
import type { TableColumn } from "react-data-table-component";
import { useCallback, useEffect, useState } from "react";
import KeyFilters from "../components/KeyFilters";
import ModelTableModal from "../components/ModelTableModal";
import Table from "../components/Table";
import {
  createProfile,
  deleteProfile,
  getModels,
  getProfiles,
  updateProfile,
} from "../lib/api";
import { errorMessage } from "../lib/errors";
import {
  allowedModels,
  descendantsOf,
  EMPTY_FILTER,
  profileAllowedModels,
  summarizeFilter,
} from "../lib/filters";
import type { KeyFilter, Model, Profile } from "../lib/types";
import {
  serverTableProps,
  useServerRows,
  type ServerTableQuery,
} from "../lib/useServerRows";

// ProfileKind distinguishes a leaf (own filters) from a derived profile
// (union of parents). The two are mutually exclusive, matching the server.
type ProfileKind = "leaf" | "derived";

// ProfileRow adds the stable table key; profiles are addressed by name.
type ProfileRow = Profile & { id: string };

// filtersValid rejects an include/exclude filter with no values, matching the
// server-side validation.
function filtersValid(f: KeyFilter): boolean {
  return f.mode === "none" || f.values.length > 0;
}

export default function Profiles() {
  // allProfiles/models back the allowed-model computations and the picker;
  // the table itself is server-paginated.
  const [allProfiles, setAllProfiles] = useState<Profile[]>([]);
  const [models, setModels] = useState<Model[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  // editing is the profile being edited, or null when creating one.
  const [editing, setEditing] = useState<Profile | null>(null);
  const [name, setName] = useState("");
  const [kind, setKind] = useState<ProfileKind>("leaf");
  const [provider, setProvider] = useState<KeyFilter>(EMPTY_FILTER);
  const [model, setModel] = useState<KeyFilter>(EMPTY_FILTER);
  const [parents, setParents] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  // previewing is the profile whose allowed-model list is shown, or null.
  const [previewing, setPreviewing] = useState<Profile | null>(null);
  // deleting is the profile awaiting delete confirmation, or null.
  const [deleting, setDeleting] = useState<Profile | null>(null);
  // pickingModels opens the model picker modal for the profile being edited.
  const [pickingModels, setPickingModels] = useState(false);

  const loadAll = useCallback(() => {
    getProfiles({ limit: 0 })
      .then((res) => setAllProfiles(res.profiles))
      .catch((e: unknown) => setError(errorMessage(e)));
    getModels({ limit: 0 })
      .then((res) => setModels(res.models))
      .catch((e: unknown) => setError(errorMessage(e)));
  }, []);

  useEffect(loadAll, [loadAll]);

  const fetchProfiles = useCallback(
    (q: ServerTableQuery) =>
      getProfiles(q).then((r) => ({
        rows: r.profiles.map((p) => ({ ...p, id: p.name })),
        total: r.total,
      })),
    [],
  );
  const table = useServerRows<ProfileRow>(fetchProfiles, {
    onError: (e) => setError(errorMessage(e)),
  });

  // reload refreshes the table page and the full lists the allowed-model
  // computations and picker rely on.
  const reloadTable = table.reload;
  const reload = useCallback(() => {
    reloadTable();
    loadAll();
  }, [reloadTable, loadAll]);

  const resetForm = () => {
    setName("");
    setKind("leaf");
    setProvider(EMPTY_FILTER);
    setModel(EMPTY_FILTER);
    setParents([]);
  };

  const openCreate = () => {
    setEditing(null);
    resetForm();
    setFormError(null);
    setOpen(true);
  };

  const openEdit = (p: Profile) => {
    setEditing(p);
    setName(p.name);
    setKind(p.parents.length > 0 ? "derived" : "leaf");
    setProvider(p.providerFilter);
    setModel(p.modelFilter);
    setParents(p.parents);
    setFormError(null);
    setOpen(true);
  };

  const closeModal = () => {
    if (busy) return;
    setOpen(false);
    setEditing(null);
    resetForm();
  };

  // requestBody builds the profile payload. A derived profile carries neutral
  // filters (its own are undefined by construction).
  const requestBody = () => {
    const derived = kind === "derived";
    return {
      name: name.trim(),
      providerFilter: derived ? EMPTY_FILTER : provider,
      modelFilter: derived ? EMPTY_FILTER : model,
      parents: derived ? parents : [],
    };
  };

  const submit = async () => {
    if (!editing) return;
    setBusy(true);
    setFormError(null);
    try {
      await updateProfile(editing.name, requestBody());
      reload();
      setOpen(false);
      setEditing(null);
      resetForm();
    } catch (err) {
      setFormError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const create = async () => {
    setBusy(true);
    setFormError(null);
    try {
      await createProfile(requestBody());
      reload();
      setOpen(false);
      resetForm();
    } catch (err) {
      setFormError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (p: Profile) => {
    setBusy(true);
    setError(null);
    try {
      await deleteProfile(p.name);
      setDeleting(null);
      reload();
    } catch (err) {
      setError(errorMessage(err));
      setDeleting(null);
    } finally {
      setBusy(false);
    }
  };

  const byName = new Map(allProfiles.map((p) => [p.name, p]));

  // Providers are the discovery sources, matching how the server gates keys.
  const providers = Array.from(
    new Set(models.map((m) => m.upstream).filter(Boolean)),
  ).sort();
  const modelIds = models.map((m) => m.gatewayId);

  // The model picker only offers models whose provider passes the provider
  // filter, so a model from an excluded provider never appears as a choice.
  // (EMPTY_FILTER for the model dimension makes allowedModels provider-only.)
  const pickerModels = allowedModels(models, provider, EMPTY_FILTER);

  // Parent candidates exclude the profile being edited and its descendants, so
  // the picker cannot build a cycle.
  const excluded = editing
    ? new Set([editing.name, ...descendantsOf(editing.name, allProfiles)])
    : new Set<string>();
  const parentCandidates = allProfiles
    .map((p) => p.name)
    .filter((n) => !excluded.has(n));

  // previewModels is the resolved allowed set for the profile being previewed.
  const previewModels = previewing
    ? profileAllowedModels(previewing, byName, models)
    : [];

  const formValid =
    name.trim() !== "" &&
    (kind === "derived"
      ? parents.length > 0
      : filtersValid(provider) && filtersValid(model));

  const columns: TableColumn<ProfileRow>[] = [
    {
      id: "name",
      name: "Name",
      selector: (p) => p.name,
      format: (p) => (p.isDefault ? `${p.name} (default)` : p.name),
      sortable: true,
      filterable: true,
    },
    {
      id: "providers",
      name: "Providers",
      selector: (p) =>
        p.parents.length > 0
          ? "—"
          : summarizeFilter(p.providerFilter, "provider", "providers"),
    },
    {
      id: "models",
      name: "Models",
      selector: (p) =>
        p.parents.length > 0
          ? "—"
          : summarizeFilter(p.modelFilter, "model", "models"),
    },
    {
      id: "basedOn",
      name: "Based on",
      selector: (p) => (p.parents.length > 0 ? p.parents.join(", ") : "—"),
    },
    {
      id: "allowed",
      name: "Allowed models",
      selector: (p) => profileAllowedModels(p, byName, models).length,
      right: true,
    },
    {
      id: "keys",
      name: "Keys",
      selector: (p) => p.keyCount,
      sortable: true,
      right: true,
    },
    {
      id: "actions",
      name: "",
      right: true,
      width: "170px",
      cell: (p) => (
        <div className="row-actions">
          <IconButton
            kind="ghost"
            size="sm"
            label={`Preview allowed models for ${p.name}`}
            onClick={() => setPreviewing(p)}
          >
            <View />
          </IconButton>
          <IconButton
            kind="ghost"
            size="sm"
            label={`Edit ${p.name}`}
            disabled={p.isDefault}
            onClick={() => openEdit(p)}
          >
            <Edit />
          </IconButton>
          <IconButton
            kind="ghost"
            size="sm"
            label={
              p.keyCount > 0
                ? `Cannot delete ${p.name}: in use by ${p.keyCount} key(s)`
                : p.childCount > 0
                  ? `Cannot delete ${p.name}: inherited by ${p.childCount} profile(s)`
                  : `Delete ${p.name}`
            }
            disabled={p.isDefault || p.keyCount > 0 || p.childCount > 0}
            onClick={() => setDeleting(p)}
          >
            <TrashCan />
          </IconButton>
        </div>
      ),
    },
  ];

  return (
    <Grid>
      <Column lg={{ span: 13, offset: 3 }}>
        <Stack gap={5}>
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
                Create profile
              </Button>
            </div>
            <Table
              columns={columns}
              data={table.rows}
              noDataComponent="No profiles yet."
              persistTableHead
              storageKey="profiles"
              {...serverTableProps(table)}
            />
          </div>
        </Stack>
      </Column>

      {/* Carbon's dialogs must not stack: a Modal never triggers another Modal,
          because the outer modal's focus trap steals focus from the inner one,
          leaving the picker's search box and page-size dropdown inert. So the
          form closes while the model picker is open and reopens on close. */}
      <Modal
        open={open && !pickingModels}
        size="md"
        modalHeading={editing ? "Edit profile" : "New profile"}
        primaryButtonText={editing ? "Save" : "Create"}
        secondaryButtonText="Cancel"
        primaryButtonDisabled={busy || !formValid}
        onRequestSubmit={editing ? submit : create}
        onRequestClose={closeModal}
        onSecondarySubmit={closeModal}
      >
        <Stack gap={5}>
          <TextInput
            id="profile-name"
            size="sm"
            labelText="Name"
            placeholder="glm-only"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <RadioButtonGroup
            legendText="Type"
            name="profile-kind"
            valueSelected={kind}
            onChange={(value) => setKind(value as ProfileKind)}
          >
            <RadioButton
              id="profile-kind-leaf"
              labelText="Own provider/model filter"
              value="leaf"
            />
            <RadioButton
              id="profile-kind-derived"
              labelText="Union of other profiles"
              value="derived"
            />
          </RadioButtonGroup>
          {kind === "leaf" ? (
            <>
              <KeyFilters
                id="profile-provider"
                title="Provider filter"
                mode={provider.mode}
                values={provider.values}
                items={providers}
                onMode={(mode) => setProvider({ mode, values: [] })}
                onValues={(values) => setProvider((p) => ({ ...p, values }))}
              />
              <KeyFilters
                id="profile-model"
                title="Model filter"
                mode={model.mode}
                values={model.values}
                items={modelIds}
                onPickValues={() => setPickingModels(true)}
                onMode={(mode) => setModel({ mode, values: [] })}
                onValues={(values) => setModel((m) => ({ ...m, values }))}
              />
            </>
          ) : (
            <MultiSelect
              id="profile-parents"
              size="sm"
              titleText="Parent profiles"
              label="Select profiles…"
              items={parentCandidates}
              selectedItems={parents}
              itemToString={(item: string) => item}
              onChange={({ selectedItems }) =>
                setParents(selectedItems as string[])
              }
            />
          )}
          {formError && (
            <InlineNotification
              kind="error"
              lowContrast
              title={editing ? "Could not update profile" : "Could not create profile"}
              subtitle={formError}
              onCloseButtonClick={() => setFormError(null)}
            />
          )}
        </Stack>
      </Modal>

      {deleting && (
        <Modal
          open
          danger
          size="sm"
          modalHeading="Delete profile"
          primaryButtonText="Delete"
          secondaryButtonText="Cancel"
          primaryButtonDisabled={busy}
          onRequestSubmit={() => remove(deleting)}
          onRequestClose={() => setDeleting(null)}
          onSecondarySubmit={() => setDeleting(null)}
        >
          <p>
            Delete profile <strong>{deleting.name}</strong>? This cannot be
            undone. No keys reference this profile and no profile inherits from
            it.
          </p>
        </Modal>
      )}

      {previewing && (
        <ModelTableModal
          title={`Allowed models — ${previewing.name}`}
          models={previewModels}
          onClose={() => setPreviewing(null)}
        />
      )}

      {pickingModels && (
        <ModelTableModal
          title="Select models"
          models={pickerModels}
          selectable
          initialSelected={model.values}
          onClose={() => setPickingModels(false)}
          onConfirm={(ids) => {
            setModel((m) => ({ ...m, values: ids }));
            setPickingModels(false);
          }}
        />
      )}
    </Grid>
  );
}
