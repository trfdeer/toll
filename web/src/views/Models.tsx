import { Edit, Pause, Play, Renew, TrashCan } from "@carbon/icons-react";
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
  deleteModel,
  disableModel,
  enableModel,
  getModels,
  refreshModels,
  setModelAlias,
} from "../lib/api";
import { errorMessage } from "../lib/errors";
import type { Model, ModelMetadata } from "../lib/types";
import {
  serverTableProps,
  useServerRows,
  type ServerTableQuery,
} from "../lib/useServerRows";

// lookup walks a dotted path into the metadata blob, returning undefined if
// any step is missing or not an object.
function lookup(
  metadata: ModelMetadata | undefined,
  path: string,
): unknown {
  let cur: unknown = metadata;
  for (const part of path.split(".")) {
    if (cur === null || typeof cur !== "object") return undefined;
    cur = (cur as Record<string, unknown>)[part];
  }
  return cur;
}

// limit returns the first present value among the fallback paths, so a column
// works across upstreams that name the same idea differently (vLLM's
// max_model_len, OpenRouter's context_length, LiteLLM's max_input_tokens, …).
// Numbers are returned as-is so the column still sorts numerically.
function limit(
  metadata: ModelMetadata | undefined,
  paths: string[],
): number | string {
  for (const path of paths) {
    const v = lookup(metadata, path);
    if (typeof v === "number") return v;
    if (typeof v === "string" && v.trim() !== "") return v;
  }
  return "—";
}

// Input falls back to the total context window: an upstream that reports only
// max_model_len/context_window is still bounded on input by that value.
const INPUT_LIMITS = [
  "max_input_tokens",
  "context_window",
  "max_model_len",
  "context_length",
  "max_context_length",
];
const OUTPUT_LIMITS = [
  "max_output_tokens",
  "max_completion_tokens",
  "max_tokens",
  "top_provider.max_completion_tokens",
];

// modelStatus reflects the effective state: a model is hidden when it or its
// provider is disabled, or when the provider was unreachable on the last sync.
function modelStatus(m: Model): Status {
  if (m.disabled) return "disabled";
  if (m.providerDisabled) return "provider disabled";
  if (!m.providerReachable) return "unreachable";
  return "active";
}

// cost renders the model's pricing when the upstream provides one.
function cost(metadata: ModelMetadata | undefined): string {
  const p = metadata?.pricing;
  if (!p || typeof p !== "object") return "—";
  const entries = Object.entries(p).filter(
    ([, v]) => v !== null && v !== "" && v !== 0,
  );
  if (entries.length === 0) return "—";
  return entries
    .map(([k, v]) =>
      typeof v === "number" ? `${k}: $${v}/M tok` : `${k}: ${v}`,
    )
    .join(", ");
}

const STATUS_VALUES = ["active", "disabled", "provider disabled", "unreachable"];

export default function Models() {
  const [error, setError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  const [flash, setFlash] = useState<string | null>(null);
  // editing is the model whose alias is being changed, or null.
  const [editing, setEditing] = useState<Model | null>(null);
  const [alias, setAlias] = useState("");
  const [aliasError, setAliasError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const fetchModels = useCallback(
    (q: ServerTableQuery) =>
      getModels(q).then((r) => ({ rows: r.models, total: r.total })),
    [],
  );
  const table = useServerRows<Model>(fetchModels, {
    onError: (e) => setError(errorMessage(e)),
  });

  // refresh forces a server-side re-discovery of every provider's catalog,
  // rather than only re-reading the current registry.
  const refresh = async () => {
    setError(null);
    setFlash(null);
    setRefreshing(true);
    try {
      const res = await refreshModels();
      table.reload();
      const warn = res.warnings?.length ? ` ${res.warnings.join("; ")}` : "";
      setFlash(
        `Re-discovered ${res.models} model(s) from ${res.providers} provider(s).${warn}`,
      );
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setRefreshing(false);
    }
  };

  const remove = async (id: number) => {
    setError(null);
    try {
      await deleteModel(id);
      table.reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const toggle = async (m: Model) => {
    setError(null);
    try {
      await (m.disabled ? enableModel(m.id) : disableModel(m.id));
      table.reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const openAlias = (m: Model) => {
    setEditing(m);
    setAlias(m.alias);
    setAliasError(null);
  };

  const closeAlias = () => {
    if (busy) return;
    setEditing(null);
    setAlias("");
    setAliasError(null);
  };

  // saveAlias sets or clears the model's custom gateway ID (empty reverts to
  // the provider-namespaced default).
  const saveAlias = async () => {
    if (!editing) return;
    setBusy(true);
    setAliasError(null);
    try {
      await setModelAlias(editing.id, alias.trim());
      table.reload();
      setEditing(null);
      setAlias("");
    } catch (err) {
      setAliasError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const columns: TableColumn<Model>[] = [
    {
      id: "gatewayId",
      name: "ID",
      selector: (m) => m.gatewayId,
      sortable: true,
      filterable: true,
      width: "160px",
      grow: 0,
    },
    {
      id: "upstream",
      name: "Provider",
      selector: (m) => m.upstream,
      sortable: true,
      filterable: true,
      width: "130px",
      grow: 0,
    },
    {
      id: "displayName",
      name: "Name",
      selector: (m) => m.displayName,
      sortable: true,
      filterable: true,
      width: "140px",
      grow: 1,
    },
    {
      id: "alias",
      name: "Alias",
      selector: (m) => m.alias || "—",
      sortable: true,
      filterable: true,
      width: "105px",
      grow: 0,
    },
    {
      id: "inputLimit",
      name: "Input limit",
      selector: (m) => limit(m.metadata, INPUT_LIMITS),
      sortable: true,
      filterable: true,
      right: true,
      width: "140px",
      grow: 0,
    },
    {
      id: "outputLimit",
      name: "Output limit",
      selector: (m) => limit(m.metadata, OUTPUT_LIMITS),
      sortable: true,
      filterable: true,
      right: true,
      width: "150px",
      grow: 0,
    },
    {
      id: "cost",
      name: "Cost",
      selector: (m) => cost(m.metadata),
      sortable: true,
      width: "120px",
      grow: 0,
    },
    {
      id: "status",
      name: "Status",
      selector: (m) => modelStatus(m),
      sortable: true,
      filterable: true,
      filterType: "set",
      filterOptions: { values: STATUS_VALUES },
      cell: (m) => <StatusTag status={modelStatus(m)} />,
      width: "120px",
      grow: 0,
    },
    {
      id: "actions",
      name: "",
      right: true,
      width: "130px",
      grow: 0,
      cell: (m) => (
        <div className="row-actions">
          {/* Always clickable: enabling a disabled model is allowed and
              reachability is evaluated separately — the badge shows
              "unreachable" while its provider is down. */}
          <IconButton
            kind="ghost"
            size="sm"
            label={`Edit alias for ${m.gatewayId}`}
            onClick={() => openAlias(m)}
          >
            <Edit />
          </IconButton>
          <IconButton
            kind="ghost"
            size="sm"
            label={
              m.disabled ? `Enable ${m.gatewayId}` : `Disable ${m.gatewayId}`
            }
            onClick={() => toggle(m)}
          >
            {m.disabled ? <Play /> : <Pause />}
          </IconButton>
          <IconButton
            kind="ghost"
            size="sm"
            label={`Remove ${m.gatewayId}`}
            onClick={() => remove(m.id)}
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
          {flash && (
            <InlineNotification
              kind="success"
              lowContrast
              title="Registry refreshed"
              subtitle={flash}
              onCloseButtonClick={() => setFlash(null)}
            />
          )}

          <div className="table-block">
            <div className="table-toolbar">
              <Button
                renderIcon={Renew}
                onClick={refresh}
                disabled={refreshing}
              >
                {refreshing ? "Refreshing…" : "Refresh"}
              </Button>
            </div>
            <Table
              columns={columns}
              data={table.rows}
              noDataComponent="No models registered yet."
              persistTableHead
              storageKey="models"
              {...serverTableProps(table)}
            />
          </div>
        </Stack>
      </Column>

      <Modal
        open={editing !== null}
        modalHeading={editing ? `Alias for ${editing.displayName}` : "Model alias"}
        primaryButtonText="Save"
        secondaryButtonText="Cancel"
        primaryButtonDisabled={busy}
        onRequestSubmit={saveAlias}
        onRequestClose={closeAlias}
        onSecondarySubmit={closeAlias}
      >
        <Stack gap={5}>
          <TextInput
            id="model-alias"
            size="sm"
            labelText="Gateway model ID"
            placeholder="gpt-4o"
            value={alias}
            onChange={(e) => setAlias(e.target.value)}
          />
          <p>
            The ID clients call this model by. Leave empty to use the default{" "}
            <code>
              {editing ? `${editing.upstream}/${editing.upstreamModelId}` : "provider/model-id"}
            </code>
            .
          </p>
          {aliasError && (
            <InlineNotification
              kind="error"
              lowContrast
              title="Could not save alias"
              subtitle={aliasError}
              onCloseButtonClick={() => setAliasError(null)}
            />
          )}
        </Stack>
      </Modal>
    </Grid>
  );
}
