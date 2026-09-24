// The typed ConnectRPC client for the toll.admin.v1 admin API. In production
// it speaks the Connect protocol to /admin/api; under `bun run dev` the
// migrated RPCs run against an in-memory transport whose mock implements the
// generated interface, so the mock cannot drift from the schema
// (proto/MIGRATION.md phase 2). Unmigrated resources answer CodeUnimplemented
// until their phase lands.
import { createClient, createRouterTransport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AdminService } from "../gen/toll/admin/v1/admin_pb";
import { mockKeyRoutes } from "./mockKeys";

const transport = import.meta.env.DEV
  ? createRouterTransport(mockKeyRoutes)
  : createConnectTransport({
      baseUrl: "/admin/api",
      useBinaryFormat: true,
    });

// admin is the typed client every migrated view calls through lib/api.
export const admin = createClient(AdminService, transport);
