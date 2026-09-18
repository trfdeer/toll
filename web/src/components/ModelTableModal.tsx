import { Checkbox, Modal } from "@carbon/react";
import { useState } from "react";
import StructuredTable from "./StructuredTable";
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
// pick the models a profile's model filter names. Selection lives in local
// state keyed by gateway ID, so it survives paging and search changes.
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

  const toggle = (gatewayID: string, checked: boolean) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (checked) next.add(gatewayID);
      else next.delete(gatewayID);
      return next;
    });
  };

  const headers = selectable
    ? ["", "Display name", "Model ID", "Provider"]
    : ["Display name", "Model ID", "Provider"];

  const rows = models.map((m) =>
    selectable
      ? [
          <Checkbox
            key={m.gatewayId}
            id={`model-pick-${m.id}`}
            labelText={`Select ${m.gatewayId}`}
            hideLabel
            checked={selected.has(m.gatewayId)}
            onChange={(_e, { checked }) => toggle(m.gatewayId, checked)}
          />,
          m.displayName,
          m.gatewayId,
          m.upstream,
        ]
      : [m.displayName, m.gatewayId, m.upstream],
  );

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
      <StructuredTable
        headers={headers}
        rows={rows}
        pageSize={10}
        pageSizes={[10, 20, 50]}
        nonSortable={selectable ? [0] : undefined}
        empty="No models match."
      />
    </Modal>
  );
}
