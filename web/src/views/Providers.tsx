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
import type { TableColumn } from "react-data-table-component";
import { useCallback, useState } from "react";
import StatusTag, { type Status } from "../components/StatusTag";
import Table from "../components/Table";
import {
  addProvider,
  deleteProvider,
  disableProvider,
  enableProvider,
  getProviders,
} from "../lib/api";
import { errorMessage } from "../lib/errors";
import type { Provider } from "../lib/types";
import {
  serverTableProps,
  useServerRows,
  type ServerTableQuery,
} from "../lib/useServerRows";

// status maps a provider onto a badge: disabled by an operator, or unreachable
// on the last discovery sync (its models are hidden either way).
function status(p: Provider): Status {
  if (p.disabled) return "disabled";
  return p.reachable ? "active" : "unreachable";
}

// ProviderRow adds the stable key the table needs; providers are addressed by
// name in the API, so that is the natural identity.
type ProviderRow = Provider & { id: string };

const STATUS_VALUES = ["active", "disabled", "unreachable"];

export default function Providers() {
  const [error, setError] = useState<string | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [name, setName] = useState("");
  const [baseURL, setBaseURL] = useState("");
  const [apiKey, setApiKey] = useState("");

  const fetchProviders = useCallback(
    (q: ServerTableQuery) =>
      getProviders(q).then((r) => ({
        rows: r.providers.map((p) => ({ ...p, id: p.name })),
        total: r.total,
      })),
    [],
  );
  const table = useServerRows<ProviderRow>(fetchProviders, {
    onError: (e) => setError(errorMessage(e)),
  });

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
      table.reload();
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
      table.reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const toggle = async (p: Provider) => {
    setError(null);
    try {
      await (p.disabled ? enableProvider(p.name) : disableProvider(p.name));
      table.reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const columns: TableColumn<ProviderRow>[] = [
    {
      id: "name",
      name: "Name",
      selector: (p) => p.name,
      sortable: true,
      filterable: true,
    },
    {
      id: "baseURL",
      name: "Base URL",
      selector: (p) => p.baseURL,
      sortable: true,
      filterable: true,
    },
    {
      id: "modelCount",
      name: "Models in registry",
      selector: (p) => p.modelCount,
      sortable: true,
      right: true,
    },
    {
      id: "status",
      name: "Status",
      selector: (p) => status(p),
      sortable: true,
      filterable: true,
      filterType: "set",
      filterOptions: { values: STATUS_VALUES },
      cell: (p) => (
        <span title={p.lastError || undefined}>
          <StatusTag status={status(p)} />
        </span>
      ),
    },
    {
      id: "actions",
      name: "",
      right: true,
      width: "120px",
      cell: (p) => (
        <div className="row-actions">
          {/* Always clickable: enabling a disabled provider is allowed and
              reachability is evaluated separately (the badge shows unreachable
              until the next successful sync). */}
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
        </div>
      ),
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

          <div className="table-block">
            <div className="table-toolbar">
              <Button renderIcon={Add} onClick={openModal}>
                Add provider
              </Button>
            </div>
            <Table
              columns={columns}
              data={table.rows}
              noDataComponent="No providers configured."
              persistTableHead
              {...serverTableProps(table)}
            />
          </div>
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
