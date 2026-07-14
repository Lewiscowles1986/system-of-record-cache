import { test, before, after } from "node:test";
import assert from "node:assert";
import { GenericContainer, StartedTestContainer } from "testcontainers";
import { SharedStoreClient } from "./store_client";
import { randomUUID } from "node:crypto";

let container: StartedTestContainer;
let redisHost: string;
let redisPort: number;

before(async () => {
  // Start the container
  container = await new GenericContainer("redis:7.0-alpine")
    .withExposedPorts(6379)
    .start();
  
  redisHost = container.getHost();
  redisPort = container.getMappedPort(6379);
});

after(async () => {
  if (container) {
    await container.stop();
  }
});

function createClient(): SharedStoreClient {
  const namespace = `test-ts-${randomUUID().slice(0, 8)}`;
  return new SharedStoreClient({
    host: redisHost,
    port: redisPort,
    namespace
  });
}

test("TypeScript Client - put, get and secondary index lookups", async () => {
  const client = createClient();
  const primary = "comp-123";
  const secondary = "acc-456";
  const payload = { companyName: "Antigravity TS", count: 42 };

  // Test put
  const putOk = await client.put(primary, secondary, payload, 10);
  assert.strictEqual(putOk, true);

  // Test get by primary
  const item = await client.get(primary);
  assert.ok(item);
  assert.strictEqual(item.primary_val, primary);
  assert.strictEqual(item.secondary_val, secondary);
  assert.deepStrictEqual(item.payload, payload);

  // Test get by secondary index
  const itemBySec = await client.getBySecondary(secondary);
  assert.ok(itemBySec);
  assert.strictEqual(itemBySec.primary_val, primary);
  assert.deepStrictEqual(itemBySec.payload, payload);

  await client.close();
});

test("TypeScript Client - delete removes secondary index", async () => {
  const client = createClient();
  const primary = "comp-789";
  const secondary = "acc-012";
  const payload = { deleteMe: true };

  await client.put(primary, secondary, payload, 10);
  const delOk = await client.delete(primary);
  assert.strictEqual(delOk, true);

  const item = await client.get(primary);
  assert.strictEqual(item, null);

  const itemBySec = await client.getBySecondary(secondary);
  assert.strictEqual(itemBySec, null);

  await client.close();
});

test("TypeScript Client - TTL expiration", async () => {
  const client = createClient();
  const primary = "comp-ttl";
  const secondary = "acc-ttl";
  const payload = { temp: true };

  await client.put(primary, secondary, payload, 1);
  
  // Wait 1.5 seconds for TTL expiration
  await new Promise((resolve) => setTimeout(resolve, 1500));

  const item = await client.get(primary);
  assert.strictEqual(item, null);

  const itemBySec = await client.getBySecondary(secondary);
  assert.strictEqual(itemBySec, null);

  await client.close();
});
