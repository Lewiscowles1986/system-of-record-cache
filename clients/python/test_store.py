import uuid
import time
import pytest
from testcontainers.redis import RedisContainer
from store_client import SharedStoreClient

@pytest.fixture(scope="module")
def redis_container():
    """Spins up an ephemeral local Redis container."""
    with RedisContainer("redis:7.0-alpine") as redis:
        yield redis

@pytest.fixture
def client(redis_container):
    """Creates a client instance pointing to the test container with a unique namespace."""
    host = redis_container.get_container_host_ip()
    port = redis_container.get_exposed_port(6379)
    namespace = f"test-{uuid.uuid4().hex[:8]}"
    return SharedStoreClient(host=host, port=int(port), namespace=namespace)

def test_put_and_get(client):
    primary = "company_100"
    secondary = "account_200"
    payload = {"company_name": "Antigravity Inc.", "status": "active"}

    # Write data
    assert client.put(primary, secondary, payload, ttl_seconds=10)

    # Read data by primary
    retrieved = client.get(primary)
    assert retrieved is not None
    assert retrieved["primary_val"] == primary
    assert retrieved["secondary_val"] == secondary
    assert retrieved["payload"] == payload

    # Read data by secondary
    retrieved_by_sec = client.get_by_secondary(secondary)
    assert retrieved_by_sec is not None
    assert retrieved_by_sec["primary_val"] == primary
    assert retrieved_by_sec["payload"] == payload

def test_delete(client):
    primary = "company_300"
    secondary = "account_400"
    payload = {"company_name": "Delete Me", "status": "temp"}

    # Write and delete
    client.put(primary, secondary, payload, ttl_seconds=10)
    assert client.delete(primary)

    # Verify it is deleted
    assert client.get(primary) is None
    assert client.get_by_secondary(secondary) is None

def test_ttl_expiration(client):
    primary = "company_500"
    secondary = "account_600"
    payload = {"temp": "data"}

    # Write with a very short TTL (1 second)
    client.put(primary, secondary, payload, ttl_seconds=1)
    
    # Wait for expiration
    time.sleep(2)

    # Verify both primary and secondary keys are expired/removed
    assert client.get(primary) is None
    assert client.get_by_secondary(secondary) is None
