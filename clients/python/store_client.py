import json
import logging
from typing import Dict, Any, Optional

import redis

logger = logging.getLogger(__name__)

class SharedStoreClient:
    def __init__(
        self,
        host: str,
        port: int = 6379,
        db: int = 0,
        ssl: bool = False,
        username: Optional[str] = None,
        iam_auth: bool = False,
        region: Optional[str] = None,
        namespace: str = "company"
    ):
        """
        Initializes the SharedStoreClient.
        
        :param host: Redis connection endpoint.
        :param port: Redis port.
        :param db: Redis database number.
        :param ssl: Use SSL/TLS.
        :param username: Redis username (e.g., 'read-write', 'read-only').
        :param iam_auth: Enables AWS IAM-based authentication.
        :param region: AWS region (required if iam_auth is True).
        :param namespace: Key prefix namespace (default: 'company').
        """
        self.host = host
        self.port = port
        self.db = db
        self.ssl = ssl
        self.username = username
        self.iam_auth = iam_auth
        self.region = region
        self.namespace = namespace
        self._client: Optional[redis.Redis] = None

    def _get_connection(self) -> redis.Redis:
        """Returns a connected Redis client instance, refreshing IAM auth if needed."""
        # For IAM authentication, we generate a fresh password token since they expire in 15m.
        password = None
        if self.iam_auth:
            password = self._generate_iam_token()

        # Rebuild connection if it is not initialized or if we are using IAM Auth (to pick up fresh token)
        if self._client is None or self.iam_auth:
            self._client = redis.Redis(
                host=self.host,
                port=self.port,
                db=self.db,
                ssl=self.ssl,
                username=self.username,
                password=password,
                decode_responses=True # Decodes response bytes to strings automatically
            )
        return self._client

    def _generate_iam_token(self) -> str:
        """Generates an AWS IAM authentication token for connecting to ElastiCache."""
        try:
            import boto3
            from botocore.auth import SigV4QueryAuth
            from botocore.awsrequest import AWSRequest
        except ImportError as e:
            raise ImportError(
                "AWS IAM Auth requires 'boto3' and 'botocore' libraries. "
                "Install them or disable 'iam_auth'."
            ) from e

        if not self.region:
            raise ValueError("AWS region must be provided if iam_auth is True")

        # Set up credentials and sign request
        session = boto3.Session()
        credentials = session.get_credentials().get_frozen_credentials()
        
        request = AWSRequest(
            method="GET",
            url=f"http://{self.host}/",
            params={"Action": "connect", "User": self.username}
        )
        
        SigV4QueryAuth(credentials, "elasticache", self.region, expires=900).add_auth(request)
        token = request.url.replace("http://", "")
        return token

    def _primary_key(self, primary_val: str) -> str:
        return f"{self.namespace}:{primary_val}"

    def _index_key(self, secondary_val: str) -> str:
        return f"{self.namespace}:index:account_id:{secondary_val}"

    def put(self, primary_val: str, secondary_val: str, payload: Dict[str, Any], ttl_seconds: int) -> bool:
        """
        Stores an item in the cache under the primary value as a Redis Hash, 
        creates a secondary lookup index key, and sets a TTL on both.
        
        :param primary_val: The primary key value (e.g. company_id).
        :param secondary_val: The secondary key value (e.g. account_id).
        :param payload: The actual data payload to store.
        :param ttl_seconds: Time to live in seconds.
        """
        if ttl_seconds <= 0:
            raise ValueError("TTL must be a positive integer")

        r = self._get_connection()
        p_key = self._primary_key(primary_val)
        
        # Store as Hash map
        r.hset(p_key, mapping={
            "secondary_index": secondary_val or "",
            "payload": json.dumps(payload)
        })
        r.expire(p_key, ttl_seconds)

        # Set secondary index mapping to the primary key
        if secondary_val:
            s_key = self._index_key(secondary_val)
            r.set(s_key, primary_val, ex=ttl_seconds)

        return True

    def get(self, primary_val: str) -> Optional[Dict[str, Any]]:
        """
        Retrieves the item payload and secondary index by primary value.
        """
        r = self._get_connection()
        p_key = self._primary_key(primary_val)
        
        data = r.hgetall(p_key)
        if not data:
            return None

        return {
            "primary_val": primary_val,
            "secondary_val": data.get("secondary_index"),
            "payload": json.loads(data.get("payload", "{}"))
        }

    def get_by_secondary(self, secondary_val: str) -> Optional[Dict[str, Any]]:
        """
        Retrieves the item by its secondary index value.
        """
        r = self._get_connection()
        s_key = self._index_key(secondary_val)
        
        primary_val = r.get(s_key)
        if not primary_val:
            return None

        return self.get(primary_val)

    def delete(self, primary_val: str) -> bool:
        """
        Deletes the item and its secondary lookup index.
        """
        r = self._get_connection()
        p_key = self._primary_key(primary_val)
        
        # Read secondary index value first so we can clean it up too
        secondary_val = r.hget(p_key, "secondary_index")
        
        # Delete primary key
        r.delete(p_key)
        
        # Delete secondary index key
        if secondary_val:
            s_key = self._index_key(secondary_val)
            r.delete(s_key)

        return True
