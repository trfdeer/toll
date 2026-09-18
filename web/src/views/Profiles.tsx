import { Add, Edit, TrashCan, View } from "@carbon/icons-react";
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
import KeyFilters from "../components/KeyFilters";
import ModelTableModal from "../components/ModelTableModal";
import PageState from "../components/PageState";
import StructuredTable from "../components/StructuredTable";
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
  EMPTY_FILTER,
  summarizeFilter,
} from "../lib/filters";
import type { KeyFilter, Model, Profile } from "../lib/types";

// filtersValid rejects an include/exclude filter with no values, matching the
// server-side validation.
function filtersValid(f: KeyFilter): boolean {
  return f.mode === "none" || f.values.length > 0;
}

export default function Profiles() {
  const [profiles, setProfiles] = useState<Profile[] | null>(null);
  const [models, setModels] = useState<Model[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  // editing is the profile being edited, or null when creating one.
  const [editing, setEditing] = useState<Profile | null>(null);
  const [name, setName] = useState("");
  const [provider, setProvider] = useState<KeyFilter>(EMPTY_FILTER);
  const [model, setModel] = useState<KeyFilter>(EMPTY_FILTER);
  const [busy, setBusy] = useState(false);
  // previewing is the profile whose allowed-model list is shown, or null.
  const [previewing, setPreviewing] = useState<Profile | null>(null);
  // deleting is the profile awaiting delete confirmation, or null.
  const [deleting, setDeleting] = useState<Profile | null>(null);
  // pickingModels opens the model picker modal for the profile being edited.
  const [pickingModels, setPickingModels] = useState(false);

  const reload = useCallback(() => {
    getProfiles()
      .then((res) => setProfiles(res.profiles))
      .catch((e: unknown) => setError(errorMessage(e)));
    getModels()
      .then(setModels)
      .catch((e: unknown) => setError(errorMessage(e)));
  }, []);

  useEffect(reload, [reload]);

  const resetForm = () => {
    setName("");
    setProvider(EMPTY_FILTER);
    setModel(EMPTY_FILTER);
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
    setProvider(p.providerFilter);
    setModel(p.modelFilter);
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
    if (!editing) return;
    setBusy(true);
    setFormError(null);
    try {
      await updateProfile(editing.name, {
        name: name.trim(),
        providerFilter: provider,
        modelFilter: model,
      });
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
      await createProfile({
        name: name.trim(),
        providerFilter: provider,
        modelFilter: model,
      });
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

  if (error && profiles === null) return <PageState error={error} />;
  if (!profiles) return <PageState />;

  // Providers are the discovery sources, matching how the server gates keys.
  const providers = Array.from(
    new Set(models.map((m) => m.upstream).filter(Boolean)),
  ).sort();
  const modelIds = models.map((m) => m.gatewayId);

  // The model picker only offers models whose provider passes the provider
  // filter, so a model from an excluded provider never appears as a choice.
  // (EMPTY_FILTER for the model dimension makes allowedModels provider-only.)
  const pickerModels = allowedModels(models, provider, EMPTY_FILTER);

  // previewModels is the resolved allowed set for the profile being previewed.
  const previewModels = previewing
    ? previewing.isDefault
      ? models
      : allowedModels(models, previewing.providerFilter, previewing.modelFilter)
    : [];

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

          <StructuredTable
            headers={[
              "Name",
              "Providers",
              "Models",
              "Allowed models",
              "Keys",
              "",
            ]}
            searchable={false}
            className="profiles-table"
            actions={
              <Button renderIcon={Add} onClick={openCreate}>
                Create profile
              </Button>
            }
            empty="No profiles yet."
            rows={profiles.map((p) => {
              const allowed = p.isDefault
                ? models.length
                : allowedModels(
                    models,
                    p.providerFilter,
                    p.modelFilter,
                  ).length;
              return [
                p.isDefault ? `${p.name} (default)` : p.name,
                summarizeFilter(p.providerFilter, "provider", "providers"),
                summarizeFilter(p.modelFilter, "model", "models"),
                allowed,
                p.keyCount,
                <>
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
                        : `Delete ${p.name}`
                    }
                    disabled={p.isDefault || p.keyCount > 0}
                    onClick={() => setDeleting(p)}
                  >
                    <TrashCan />
                  </IconButton>
                </>,
              ];
            })}
          />
        </Stack>
      </Column>

      <Modal
        open={open}
        size="md"
        modalHeading={editing ? "Edit profile" : "New profile"}
        primaryButtonText={editing ? "Save" : "Create"}
        secondaryButtonText="Cancel"
        primaryButtonDisabled={
          busy ||
          !name.trim() ||
          !filtersValid(provider) ||
          !filtersValid(model)
        }
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
            undone. No keys reference this profile.
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
