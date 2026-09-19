import { Modal } from "@carbon/react";
import type { TableColumn } from "react-data-table-component";
import { useState } from "react";
import Table from "./Table";
import type { Model } from "../lib/types";

export interface ModelTableModalProps {
  title: string;
  models: Model[];
  /** When true, rows get checkboxes and an Apply action; otherwise read-only. */
  selectable?: boolean;
  /** Gateway IDs checked when the modal opens (selectable mode). */
  initialSelected?: string[];
  onClose: () => void;
  /** Called with the chosen gateway IDs on Apply (selectable mode). */
  onConfirm?: (gatewayIds: string[]) => void;
}

// ModelTableModal shows a model list in a paginated, searchable table. It is
// used read-only to preview a profile's allowed models, and with selection to
// pick the models a profile's model filter names. Selection is the table's
// built-in row selection (keyed by gateway ID), so it survives paging and
// filtering.
export default function ModelTableModal({
  title,
  models,
  selectable = false,
  initialSelected = [],
  onClose,
  onConfirm,
}: ModelTableModalProps) {
  const [selected, setSelected] = useState<Set<string>>(
    () => new Set(initialSelected),
  );

  const columns: TableColumn<Model>[] = [
    {
      id: "displayName",
      name: "Display name",
      selector: (m) => m.displayName,
      sortable: true,
      filterable: true,
    },
    {
      id: "gatewayId",
      name: "Model ID",
      selector: (m) => m.gatewayId,
      sortable: true,
      filterable: true,
    },
    {
      id: "upstream",
      name: "Provider",
      selector: (m) => m.upstream,
      sortable: true,
      filterable: true,
      filterType: "set",
    },
  ];

  return (
    <Modal
      open
      size="lg"
      passiveModal={!selectable}
      modalHeading={title}
      primaryButtonText={selectable ? "Apply" : undefined}
      secondaryButtonText={selectable ? "Cancel" : undefined}
      onRequestSubmit={
        selectable ? () => onConfirm?.(Array.from(selected)) : undefined
      }
      onRequestClose={onClose}
      onSecondarySubmit={onClose}
    >
      <Table
        keyField="gatewayId"
        columns={columns}
        data={models}
        noDataComponent="No models match."
        selectableRows={selectable}
        selectableRowsHighlight={selectable}
        selectedRows={
          selectable
            ? models.filter((m) => selected.has(m.gatewayId))
            : undefined
        }
        onSelectedRowsChange={
          selectable
            ? ({ selectedRows }) =>
                setSelected(new Set(selectedRows.map((m) => m.gatewayId)))
            : undefined
        }
      />
    </Modal>
  );
}
