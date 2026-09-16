-- Gives a Brain the workflow tools the host exposes.
--
-- It has no data ports: it is bound to a Brain by a tool edge and the data flow
-- never touches it. What it returns is the list of tool names it made
-- available, so that someone looking at the node can see what the Brain was
-- given rather than having to infer it.
return {
  type = "zyvroTools",
  label = "Workflow Tools",
  category = "Agent",
  description = "Gives a Brain the MCP tools: list, inspect and run your other workflows",
  inputs = {},
  outputs = {},
  toolOnly = true,
  config = {},

  run = function(ctx)
    return ctx.agentTools()
  end,
}
