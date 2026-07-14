defmodule SharedStoreClientTest do
  use ExUnit.Case, async: true
  import Testcontainers.ExUnit

  # Automatically start an ephemeral Redis container for this test suite
  container(
    :redis,
    Testcontainers.RedisContainer.new()
    |> Testcontainers.RedisContainer.with_image("redis:7.0-alpine")
  )

  setup %{redis: redis} do
    host = Testcontainers.get_host()
    port = Testcontainers.Container.mapped_port(redis, 6379)
    # Generate unique namespace to support parallel test execution
    namespace = "test-ex-#{:erlang.unique_integer([:positive])}"

    {:ok, client} = SharedStoreClient.connect(host: host, port: port, namespace: namespace)

    on_exit(fn ->
      SharedStoreClient.disconnect(client)
    end)

    {:ok, client: client}
  end

  test "put, get and secondary index queries", %{client: client} do
    primary = "company_999"
    secondary = "account_888"
    payload = %{"name" => "Antigravity Elixir", "tags" => ["tech", "cloud"]}

    # Put data
    assert :ok == SharedStoreClient.put(client, primary, secondary, payload, 10)

    # Get data by primary key
    assert {:ok, retrieved} = SharedStoreClient.get(client, primary)
    assert retrieved["primary_val"] == primary
    assert retrieved["secondary_val"] == secondary
    assert retrieved["payload"] == payload

    # Get data by secondary index
    assert {:ok, retrieved_by_sec} = SharedStoreClient.get_by_secondary(client, secondary)
    assert retrieved_by_sec["primary_val"] == primary
    assert retrieved_by_sec["payload"] == payload
  end

  test "delete removes index and data", %{client: client} do
    primary = "company_delete"
    secondary = "account_delete"
    payload = %{"to_delete" => true}

    assert :ok == SharedStoreClient.put(client, primary, secondary, payload, 10)
    assert :ok == SharedStoreClient.delete(client, primary)

    assert {:ok, nil} == SharedStoreClient.get(client, primary)
    assert {:ok, nil} == SharedStoreClient.get_by_secondary(client, secondary)
  end

  test "TTL expiration removes data", %{client: client} do
    primary = "company_ttl"
    secondary = "account_ttl"
    payload = %{"temporary" => true}

    # Write with 1 second TTL
    assert :ok == SharedStoreClient.put(client, primary, secondary, payload, 1)

    # Wait 1.5 seconds for TTL expiration
    Process.sleep(1500)

    assert {:ok, nil} == SharedStoreClient.get(client, primary)
    assert {:ok, nil} == SharedStoreClient.get_by_secondary(client, secondary)
  end
end
