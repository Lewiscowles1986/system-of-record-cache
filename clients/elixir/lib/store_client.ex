defmodule SharedStoreClient do
  @moduledoc """
  Elixir client for the shared Redis key-value store.
  Supports primary/secondary keys and payload separation with mandatory TTLs.
  """

  defstruct [:conn, :namespace]

  @type t :: %__MODULE__{
          conn: GenServer.server(),
          namespace: String.t()
        }

  @doc """
  Starts a connection to Redis and returns a SharedStoreClient struct.
  
  Options:
    * `:host` - Redis hostname (required)
    * `:port` - Redis port (default: 6379)
    * `:database` - Redis database (default: 0)
    * `:ssl` - Use SSL/TLS (default: false)
    * `:username` - Redis username
    * `:password` - Redis password (or AWS IAM connection token)
    * `:namespace` - Prefix namespace (default: "company")
  """
  def connect(opts) do
    host = Keyword.fetch!(opts, :host)
    port = Keyword.get(opts, :port, 6379)
    database = Keyword.get(opts, :database, 0)
    ssl = Keyword.get(opts, :ssl, false)
    username = Keyword.get(opts, :username)
    password = Keyword.get(opts, :password)
    namespace = Keyword.get(opts, :namespace, "company")

    redix_opts = [
      host: host,
      port: port,
      database: database,
      ssl: ssl
    ]

    redix_opts = if username, do: Keyword.put(redix_opts, :username, username), else: redix_opts
    redix_opts = if password, do: Keyword.put(redix_opts, :password, password), else: redix_opts

    case Redix.start_link(redix_opts) do
      {:ok, conn} ->
        {:ok, %__MODULE__{conn: conn, namespace: namespace}}

      {:error, reason} ->
        {:error, reason}
    end
  end

  @doc """
  Disconnects the Redis connection.
  """
  def disconnect(%__MODULE__{conn: conn}) do
    if Process.alive?(conn) do
      Redix.stop(conn)
    else
      :ok
    end
  end

  @doc """
  Writes the item data as a Redis Hash and sets the secondary index mapping with a TTL.
  """
  @spec put(t(), String.t(), String.t() | nil, map(), integer()) :: :ok | {:error, any()}
  def put(%__MODULE__{} = client, primary_val, secondary_val, payload, ttl_seconds)
      when is_integer(ttl_seconds) and ttl_seconds > 0 do
    p_key = primary_key(client, primary_val)
    payload_json = Jason.encode!(payload)

    # Prepare command pipeline
    commands = [
      ["HSET", p_key, "secondary_index", secondary_val || "", "payload", payload_json],
      ["EXPIRE", p_key, to_string(ttl_seconds)]
    ]

    commands =
      if secondary_val do
        s_key = index_key(client, secondary_val)
        commands ++ [["SET", s_key, primary_val, "EX", to_string(ttl_seconds)]]
      else
        commands
      end

    case Redix.pipeline(client.conn, commands) do
      {:ok, _results} -> :ok
      {:error, reason} -> {:error, reason}
    end
  end

  @doc """
  Retrieves the item by its primary value.
  """
  @spec get(t(), String.t()) :: {:ok, map() | nil} | {:error, any()}
  def get(%__MODULE__{} = client, primary_val) do
    p_key = primary_key(client, primary_val)

    case Redix.command(client.conn, ["HGETALL", p_key]) do
      {:ok, []} ->
        {:ok, nil}

      {:ok, list} ->
        map = list_to_map(list)
        secondary_val = Map.get(map, "secondary_index")
        payload = Jason.decode!(Map.get(map, "payload", "{}"))

        {:ok,
         %{
           "primary_val" => primary_val,
           "secondary_val" => if(secondary_val == "", do: nil, else: secondary_val),
           "payload" => payload
         }}

      {:error, reason} ->
        {:error, reason}
    end
  end

  @doc """
  Retrieves the item by its secondary index value.
  """
  @spec get_by_secondary(t(), String.t()) :: {:ok, map() | nil} | {:error, any()}
  def get_by_secondary(%__MODULE__{} = client, secondary_val) do
    s_key = index_key(client, secondary_val)

    case Redix.command(client.conn, ["GET", s_key]) do
      {:ok, nil} ->
        {:ok, nil}

      {:ok, primary_val} ->
        get(client, primary_val)

      {:error, reason} ->
        {:error, reason}
    end
  end

  @doc """
  Deletes the primary item and its secondary index from Redis.
  """
  @spec delete(t(), String.t()) :: :ok | {:error, any()}
  def delete(%__MODULE__{} = client, primary_val) do
    p_key = primary_key(client, primary_val)

    case Redix.command(client.conn, ["HGET", p_key, "secondary_index"]) do
      {:ok, secondary_val} ->
        commands = [["DEL", p_key]]

        commands =
          if secondary_val && secondary_val != "" do
            s_key = index_key(client, secondary_val)
            commands ++ [["DEL", s_key]]
          else
            commands
          end

        case Redix.pipeline(client.conn, commands) do
          {:ok, _results} -> :ok
          {:error, reason} -> {:error, reason}
        end

      {:error, reason} ->
        {:error, reason}
    end
  end

  # Helper to construct primary keys
  defp primary_key(client, primary_val), do: "#{client.namespace}:#{primary_val}"

  # Helper to construct secondary index keys
  defp index_key(client, secondary_val), do: "#{client.namespace}:index:account_id:#{secondary_val}"

  # Helper to convert Redix flat list response from HGETALL to a map
  defp list_to_map(list) do
    list
    |> Enum.chunk_every(2)
    |> Map.new(fn [k, v] -> {k, v} end)
  end
end
