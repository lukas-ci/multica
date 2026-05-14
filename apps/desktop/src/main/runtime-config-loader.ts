import { app } from "electron";
import { readFile } from "fs/promises";
import { join } from "path";
import {
  DEFAULT_RUNTIME_CONFIG,
  parseRuntimeConfig,
  runtimeConfigFromDevEnv,
  type RuntimeConfig,
  type RuntimeConfigEnv,
  type RuntimeConfigResult,
} from "../shared/runtime-config";

export async function loadRuntimeConfig(options: {
  isDev: boolean;
  env: RuntimeConfigEnv;
  configPath?: string;
}): Promise<RuntimeConfigResult> {
  if (options.isDev) {
    if (options.env.apiUrl) {
      // VITE_API_URL is set at build time — use it directly, skip desktop.json.
      // This allows the source-built Canary to use a baked-in LAN backend
      // while the DMG app reads desktop.json for its own backend independently.
      try {
        return { ok: true, config: runtimeConfigFromDevEnv(options.env) };
      } catch (err) {
        return { ok: false, error: { message: errorMessage(err) } };
      }
    }
    // No VITE_API_URL — try desktop.json first, fall back to dev defaults.
    const devConfigPath = options.configPath ?? desktopConfigPath();
    try {
      const raw = await readFile(devConfigPath, "utf-8");
      const config = parseRuntimeConfig(raw);
      if (config.apiUrl) {
        return { ok: true, config };
      }
    } catch { /* fall through to dev env */ }
    try {
      return { ok: true, config: runtimeConfigFromDevEnv(options.env) };
    } catch (err) {
      return { ok: false, error: { message: errorMessage(err) } };
    }
  }

  const configPath = options.configPath ?? desktopConfigPath();
  try {
    const raw = await readFile(configPath, "utf-8");
    return { ok: true, config: parseRuntimeConfig(raw) };
  } catch (err) {
    if (isMissingFileError(err)) {
      return { ok: true, config: { ...DEFAULT_RUNTIME_CONFIG } };
    }
    return {
      ok: false,
      error: {
        message: `Invalid ${configPath}: ${errorMessage(err)}`,
      },
    };
  }
}

export function desktopConfigPath(): string {
  return join(app.getPath("home"), ".multica", "desktop.json");
}

function isMissingFileError(err: unknown): boolean {
  return Boolean(
    err &&
      typeof err === "object" &&
      "code" in err &&
      (err as NodeJS.ErrnoException).code === "ENOENT",
  );
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export type { RuntimeConfig, RuntimeConfigResult };
