import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { join } from "path";
import { tmpdir } from "os";
import { mkdirSync, writeFileSync, existsSync, rmSync, readFileSync } from "fs";

import {
  deriveProfileName,
  healthPortForProfile,
  normalizeUrl,
  urlsMatch,
  profileArgs,
  profileConfigPath,
  profileDir,
  desktopSpawnEnv,
  resolveActiveProfile,
  syncToken,
  startDaemon,
  __setTargetApiBaseUrl,
  __setActiveProfile,
  __setTestHomeDir,
  __setTestCliBinary,
  __setTestExecFile,
} from "./daemon-manager";

// Describe and tests for pure helpers remain the same.
describe("deriveProfileName", () => {
  it("replaces colons in hostname (dots preserved)", () => {
    expect(deriveProfileName("http://192.168.3.172:8080")).toBe(
      "desktop-192.168.3.172-8080",
    );
  });
  it("handles hostname without port", () => {
    expect(deriveProfileName("http://api.multica.ai")).toBe(
      "desktop-api.multica.ai",
    );
  });
  it("handles https with default port stripped", () => {
    expect(deriveProfileName("https://server.example.com:443")).toBe(
      "desktop-server.example.com",
    );
  });
  it("returns 'desktop' for invalid URL", () => {
    expect(deriveProfileName("not-a-url")).toBe("desktop");
  });
});

describe("healthPortForProfile", () => {
  it("returns default port for empty profile", () => {
    expect(healthPortForProfile("")).toBe(19514);
  });
  it("derives stable port for named profile", () => {
    const p1 = healthPortForProfile("desktop-api-multica-ai");
    const p2 = healthPortForProfile("desktop-api-multica-ai");
    expect(p1).toBe(p2);
    expect(p1).not.toBe(19514);
  });
});

describe("normalizeUrl", () => {
  it("strips trailing slash", () => {
    expect(normalizeUrl("http://host:8080/")).toBe("http://host:8080");
  });
  it("lowercases host", () => {
    expect(normalizeUrl("HTTP://HOST:8080")).toBe("http://host:8080");
  });
  it("strips path", () => {
    expect(normalizeUrl("http://host:8080/api/mcp")).toBe("http://host:8080");
  });
  it("returns empty for empty input", () => {
    expect(normalizeUrl("")).toBe("");
  });
});

describe("urlsMatch", () => {
  it("matches same host:port", () => {
    expect(urlsMatch("http://host:8080", "http://host:8080")).toBe(true);
  });
  it("matches ignoring trailing slashes", () => {
    expect(urlsMatch("http://host:8080/", "http://host:8080")).toBe(true);
  });
  it("does not match different hosts", () => {
    expect(urlsMatch("http://a:8080", "http://b:8080")).toBe(false);
  });
  it("does not match empty input", () => {
    expect(urlsMatch("http://a:8080", "")).toBe(false);
  });
});

describe("profileArgs", () => {
  it("returns --profile for named profile", () => {
    expect(profileArgs({ name: "desktop-test", port: 19515 })).toEqual([
      "--profile", "desktop-test",
    ]);
  });
  it("returns empty array for empty profile name", () => {
    expect(profileArgs({ name: "", port: 19514 })).toEqual([]);
  });
});

describe("desktopSpawnEnv", () => {
  beforeEach(() => {
    delete process.env.MULTICA_KNOWLEDGE_MCP_URL;
  });
  it("sets MULTICA_LAUNCHED_BY to desktop", () => {
    expect(desktopSpawnEnv().MULTICA_LAUNCHED_BY).toBe("desktop");
  });
  it("strips inherited MULTICA_KNOWLEDGE_MCP_URL from spawn env", () => {
    process.env.MULTICA_KNOWLEDGE_MCP_URL = "http://evil:9999/api/mcp";
    expect(desktopSpawnEnv().MULTICA_KNOWLEDGE_MCP_URL).toBeUndefined();
  });
  it("preserves existing process env vars", () => {
    expect(desktopSpawnEnv().PATH).toBeDefined();
  });
});

describe("resolveActiveProfile", () => {
  beforeEach(() => {
    __setTargetApiBaseUrl("http://192.168.3.172:18080");
    __setActiveProfile(null);
  });
  afterEach(() => {
    __setTargetApiBaseUrl(null);
    __setActiveProfile(null);
  });
  it("returns Desktop profile name from target URL", async () => {
    const profile = await resolveActiveProfile();
    expect(profile.name).toBe("desktop-192.168.3.172-18080");
  });
  it("returns default profile when targetApiBaseUrl is not set", async () => {
    __setTargetApiBaseUrl(null);
    const profile = await resolveActiveProfile();
    expect(profile.name).toBe("");
    expect(profile.port).toBe(19514);
  });
});

describe("profile utility paths", () => {
  it("profileDir returns default path for empty profile", () => {
    const dir = profileDir("");
    expect(dir).toContain(".multica");
    expect(dir.endsWith(".multica")).toBe(true);
  });
  it("profileDir returns profile subdir for named profile", () => {
    const dir = profileDir("desktop-test");
    expect(dir).toContain(".multica/profiles/desktop-test");
  });
  it("profileConfigPath returns correct path", () => {
    const path = profileConfigPath("desktop-test");
    expect(path).toContain(".multica/profiles/desktop-test/config.json");
  });
});

// ----- Integration tests with testHomeDir seam -----

function makeTempHome(): string {
  return join(tmpdir(), "multica-int-" + Date.now() + "-" + Math.random().toString(36).slice(2, 6));
}

describe("resolveActiveProfile with testHomeDir", () => {
  let tempHome: string;

  beforeEach(() => {
    tempHome = makeTempHome();
    mkdirSync(tempHome, { recursive: true });
    __setTestHomeDir(tempHome);
    __setTargetApiBaseUrl("http://192.168.3.172:18080");
    __setActiveProfile(null);
  });

  afterEach(() => {
    rmSync(tempHome, { recursive: true, force: true });
    __setTestHomeDir(null);
    __setTargetApiBaseUrl(null);
    __setActiveProfile(null);
  });

  it("writes server_url to Desktop-owned profile config.json", async () => {
    const profile = await resolveActiveProfile();
    expect(profile.name).toBe("desktop-192.168.3.172-18080");

    const cfgPath = join(tempHome, ".multica", "profiles", profile.name, "config.json");
    expect(existsSync(cfgPath)).toBe(true);
    const contents = JSON.parse(readFileSync(cfgPath, "utf-8"));
    expect(contents.server_url).toBe("http://192.168.3.172:18080");
  });

  it("preserves existing token when server_url matches", async () => {
    // Pre-write config with token
    const cfgDir = join(tempHome, ".multica", "profiles", "desktop-192.168.3.172-18080");
    mkdirSync(cfgDir, { recursive: true });
    writeFileSync(join(cfgDir, "config.json"), JSON.stringify({
      server_url: "http://192.168.3.172:18080",
      token: "mul_existing_pat",
    }));

    await resolveActiveProfile();

    const contents = JSON.parse(readFileSync(join(cfgDir, "config.json"), "utf-8"));
    expect(contents.token).toBe("mul_existing_pat"); // preserved
    expect(contents.server_url).toBe("http://192.168.3.172:18080");
  });
});

describe("syncToken with testHomeDir", () => {
  let tempHome: string;

  beforeEach(() => {
    tempHome = makeTempHome();
    mkdirSync(tempHome, { recursive: true });
    __setTestHomeDir(tempHome);
    __setTargetApiBaseUrl("http://192.168.3.172:18080");
    __setActiveProfile(null);

    // Mock fetch for mintPat
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ token: "mul_fresh_minted_pat" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });

  afterEach(() => {
    rmSync(tempHome, { recursive: true, force: true });
    __setTestHomeDir(null);
    __setTargetApiBaseUrl(null);
    __setActiveProfile(null);
    vi.restoreAllMocks();
  });

  it("mints and writes PAT to profile config on first call", async () => {
    await resolveActiveProfile(); // create profile dir + set server_url
    await syncToken("some-jwt-token", "user-1");

    const cfgDir = join(tempHome, ".multica", "profiles", "desktop-192.168.3.172-18080");
    const contents = JSON.parse(readFileSync(join(cfgDir, "config.json"), "utf-8"));
    expect(contents.token).toBe("mul_fresh_minted_pat");
    expect(contents.server_url).toBe("http://192.168.3.172:18080");
  });
});

describe("startDaemon with testHomeDir and testCliBinary", () => {
  let tempHome: string;

  beforeEach(() => {
    tempHome = makeTempHome();
    mkdirSync(tempHome, { recursive: true });
    __setTestHomeDir(tempHome);
    __setTargetApiBaseUrl("http://192.168.3.172:18080");
    __setActiveProfile(null);
    __setTestCliBinary("/fake/multica");

    // Mock health probe to return "not running"
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(null, { status: 503 }),
    );
  });

  afterEach(() => {
    rmSync(tempHome, { recursive: true, force: true });
    __setTestHomeDir(null);
    __setTargetApiBaseUrl(null);
    __setActiveProfile(null);
    __setTestCliBinary(null);
    __setTestExecFile(null);
    vi.restoreAllMocks();
  });

  it("starts daemon with --profile and clean env", async () => {
    let capturedArgs: string[] = [];
    let capturedEnv: Record<string, string> = {};

    __setTestExecFile((_bin: string, args: string[], opts: any, cb: Function) => {
      capturedArgs = args;
      capturedEnv = opts.env;
      cb(null, "", "");
      return { on: vi.fn() };
    });

    const result = await startDaemon();
    expect(result.success).toBe(true);

    expect(capturedArgs).toContain("daemon");
    expect(capturedArgs).toContain("start");
    expect(capturedArgs).toContain("--profile");
    expect(capturedArgs).toContain("desktop-192.168.3.172-18080");
    expect(capturedEnv.MULTICA_LAUNCHED_BY).toBe("desktop");
    expect(capturedEnv.MULTICA_KNOWLEDGE_MCP_URL).toBeUndefined();
  });
});
