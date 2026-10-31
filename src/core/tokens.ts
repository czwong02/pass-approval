import { createHash, randomBytes, randomInt } from "crypto";

export function newToken(): string {
    return randomBytes(16).toString("base64url");
}

export function hashToken(value: string): string {
    return createHash("sha256").update(value).digest("hex");
}

export function newLinkCode(): string {
    return String(randomInt(0, 1_000_000)).padStart(6, "0");
}

export function newId(prefix: string): string {
    return prefix + randomBytes(12).toString("base64url");
}