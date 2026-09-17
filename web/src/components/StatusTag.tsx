import { Tag } from "@carbon/react";

// Status values shared by the keys, models and providers tables.
export type Status =
  | "active"
  | "paused"
  | "revoked"
  | "disabled"
  | "unreachable"
  | "provider disabled";

const STATUS_TYPES: Record<Status, "green" | "gray" | "red" | "magenta"> = {
  active: "green",
  paused: "gray",
  revoked: "red",
  disabled: "gray",
  unreachable: "magenta",
  "provider disabled": "gray",
};

// StatusTag renders a status as a compact Carbon tag with a consistent color.
export default function StatusTag({ status }: { status: Status }) {
  return (
    <Tag type={STATUS_TYPES[status]} size="sm">
      {status}
    </Tag>
  );
}
