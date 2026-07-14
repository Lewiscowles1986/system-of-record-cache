import Redis, { RedisOptions } from "ioredis";

export interface ClientOptions {
  host: string;
  port?: number;
  db?: number;
  ssl?: boolean;
  username?: string;
  iamAuth?: boolean;
  region?: string;
  namespace?: string;
}

export interface StoreItem {
  primary_val: string;
  secondary_val: string | null;
  payload: Record<string, any>;
}

export class SharedStoreClient {
  private client: Redis | null = null;
  private readonly host: string;
  private readonly port: number;
  private readonly db: number;
  private readonly ssl: boolean;
  private readonly username?: string;
  private readonly iamAuth: boolean;
  private readonly region?: string;
  private readonly namespace: string;

  constructor(options: ClientOptions) {
    this.host = options.host;
    this.port = options.port ?? 6379;
    this.db = options.db ?? 0;
    this.ssl = options.ssl ?? false;
    this.username = options.username;
    this.iamAuth = options.iamAuth ?? false;
    this.region = options.region;
    this.namespace = options.namespace ?? "company";
  }

  /**
   * Initializes or returns the connection pool.
   * Leverages ioredis dynamic password function if IAM auth is enabled.
   */
  private getConnection(): Redis {
    if (this.client) {
      return this.client;
    }

    const redisOptions: RedisOptions = {
      host: this.host,
      port: this.port,
      db: this.db,
      username: this.username,
    };

    if (this.ssl) {
      redisOptions.tls = {};
    }

    if (this.iamAuth) {
      // ioredis supports a function for password, which it executes on every connection attempt.
      // This is perfect for SigV4 tokens that expire in 15 minutes.
      redisOptions.password = () => this.generateIamToken();
    }

    this.client = new Redis(redisOptions);
    return this.client;
  }

  /**
   * Generates a SigV4 token for ElastiCache IAM authentication.
   */
  private async generateIamToken(): Promise<string> {
    if (!this.region) {
      throw new Error("AWS region must be defined when using IAM Auth");
    }

    try {
      // Dynamically load the AWS SDK components to avoid forcing SDK dependency for local runs
      const { SignatureV4 } = await import("@aws-sdk/signature-v4");
      const { defaultProvider } = await import("@aws-sdk/credential-provider-node");
      const { Sha256 } = await import("@aws-crypto/sha256-js");

      const credentials = await defaultProvider()();
      const signer = new SignatureV4({
        credentials,
        region: this.region,
        service: "elasticache",
        sha256: Sha256,
      });

      const request = {
        method: "GET",
        protocol: "http:",
        hostname: this.host,
        port: this.port,
        path: "/",
        headers: {
          host: this.host,
        },
        query: {
          Action: "connect",
          User: this.username ?? "",
        },
      };

      const signedRequest = await signer.sign(request, {
        signingDate: new Date(),
        // 15 minutes in seconds
        expiresIn: 900,
      });

      // Assemble SigV4 token from query parameters
      const url = new URL(`http://${this.host}`);
      for (const [key, value] of Object.entries(signedRequest.query ?? {})) {
        if (value !== undefined) {
          url.searchParams.append(key, String(value));
        }
      }
      for (const [key, value] of Object.entries(signedRequest.headers ?? {})) {
        if (value !== undefined) {
          url.searchParams.append(key, String(value));
        }
      }

      return url.toString().replace("http://", "");
    } catch (err: any) {
      throw new Error(`Failed to generate AWS IAM auth token: ${err.message}. Ensure @aws-sdk packages are installed.`);
    }
  }

  private primaryKey(primaryVal: string): string {
    return `${this.namespace}:${primaryVal}`;
  }

  private indexKey(secondaryVal: string): string {
    return `${this.namespace}:index:account_id:${secondaryVal}`;
  }

  /**
   * Writes the data as a Redis Hash and updates the secondary index with a TTL.
   */
  async put(
    primaryVal: string,
    secondaryVal: string | null,
    payload: Record<string, any>,
    ttlSeconds: number
  ): Promise<boolean> {
    if (ttlSeconds <= 0) {
      throw new Error("TTL must be a positive integer");
    }

    const redis = this.getConnection();
    const pKey = this.primaryKey(primaryVal);

    // Save as hash map
    await redis.hset(pKey, {
      secondary_index: secondaryVal ?? "",
      payload: JSON.stringify(payload),
    });
    await redis.expire(pKey, ttlSeconds);

    // Update secondary index key pointing to the primary value
    if (secondaryVal) {
      const sKey = this.indexKey(secondaryVal);
      await redis.set(sKey, primaryVal, "EX", ttlSeconds);
    }

    return true;
  }

  /**
   * Reads data by primary key value.
   */
  async get(primaryVal: string): Promise<StoreItem | null> {
    const redis = this.getConnection();
    const pKey = this.primaryKey(primaryVal);

    const data = await redis.hgetall(pKey);
    if (!data || Object.keys(data).length === 0) {
      return null;
    }

    return {
      primary_val: primaryVal,
      secondary_val: data.secondary_index || null,
      payload: JSON.parse(data.payload || "{}"),
    };
  }

  /**
   * Reads data using the secondary index key value.
   */
  async getBySecondary(secondaryVal: string): Promise<StoreItem | null> {
    const redis = this.getConnection();
    const sKey = this.indexKey(secondaryVal);

    const primaryVal = await redis.get(sKey);
    if (!primaryVal) {
      return null;
    }

    return this.get(primaryVal);
  }

  /**
   * Deletes the main hash record and its secondary index record.
   */
  async delete(primaryVal: string): Promise<boolean> {
    const redis = this.getConnection();
    const pKey = this.primaryKey(primaryVal);

    const secondaryVal = await redis.hget(pKey, "secondary_index");

    await redis.del(pKey);
    if (secondaryVal) {
      const sKey = this.indexKey(secondaryVal);
      await redis.del(sKey);
    }

    return true;
  }

  /**
   * Closes the active Redis connection.
   */
  async close(): Promise<void> {
    if (this.client) {
      await this.client.quit();
      this.client = null;
    }
  }
}
