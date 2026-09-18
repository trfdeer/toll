import {
  Button,
  FilterableMultiSelect,
  MultiSelect,
  Select,
  SelectItem,
  Stack,
} from "@carbon/react";
import type { FilterMode } from "../lib/types";

// How each filter mode constrains a dimension.
const MODES: ReadonlyArray<{ value: FilterMode; label: string }> = [
  { value: "none", label: "No filter" },
  { value: "include", label: "Only selected" },
  { value: "exclude", label: "All except selected" },
];

function isFilterMode(value: string): value is FilterMode {
  return MODES.some((m) => m.value === value);
}

export interface KeyFiltersProps {
  id: string;
  title: string;
  mode: FilterMode;
  values: string[];
  items: string[];
  onMode: (mode: FilterMode) => void;
  onValues: (values: string[]) => void;
  filterable?: boolean;
  itemToString?: (item: string) => string;
  /**
   * When set, the value picker becomes a button that opens a modal instead of
   * an inline multiselect. Use it when the item list can be very large.
   */
  onPickValues?: () => void;
}

// KeyFilters edits one dimension: a mode dropdown plus a value picker shown
// only when the mode is inclusive or exclusive.
export default function KeyFilters({
  id,
  title,
  mode,
  values,
  items,
  onMode,
  onValues,
  filterable = false,
  itemToString = (x) => x,
  onPickValues,
}: KeyFiltersProps) {
  const pickerProps = {
    id: `${id}-values`,
    size: "sm" as const,
    titleText: "Values",
    label: "Select…",
    items,
    selectedItems: values,
    itemToString,
    onChange: ({ selectedItems }: { selectedItems: string[] }) =>
      onValues(selectedItems),
  };
  return (
    <Stack gap={4}>
      <Select
        id={`${id}-mode`}
        size="sm"
        labelText={title}
        value={mode}
        onChange={(e) => {
          const next = e.target.value;
          if (isFilterMode(next)) onMode(next);
        }}
      >
        {MODES.map((m) => (
          <SelectItem key={m.value} value={m.value} text={m.label} />
        ))}
      </Select>
      {mode !== "none" &&
        (onPickValues ? (
          <Button size="sm" kind="tertiary" onClick={onPickValues}>
            {values.length > 0 ? `${values.length} selected` : "Select…"}
          </Button>
        ) : filterable ? (
          <FilterableMultiSelect {...pickerProps} />
        ) : (
          <MultiSelect {...pickerProps} />
        ))}
    </Stack>
  );
}
