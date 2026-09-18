import { Column, Grid, InlineLoading, InlineNotification } from "@carbon/react";

// PageState renders a view's initial load or load-failure state inside the
// same Grid/Column wrapper the views use for their content. Returning these
// states bare would start at the content edge and sit behind the persistent
// side navigation, hiding the error text.
export default function PageState({ error }: { error?: string | null }) {
  return (
    <Grid>
      <Column lg={{ span: 13, offset: 3 }}>
        {error ? (
          <InlineNotification
            kind="error"
            lowContrast
            hideCloseButton
            title="Failed to load"
            subtitle={error}
          />
        ) : (
          <InlineLoading description="Loading…" />
        )}
      </Column>
    </Grid>
  );
}
