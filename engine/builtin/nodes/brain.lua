-- An agent that works towards a goal by calling the nodes bound to it.
--
-- The nodes it can call are the ones joined to it by a tool edge, which is not
-- a data dependency: the Brain decides at run time which of them to run and
-- with what. That is why this is the widest capability in the product — a Brain
-- reaches node types its own pack was never granted.
--
-- The goal is this node's setting, or the text arriving on its input.
return {
  type = "brain",
  label = "Brain",
  category = "Agent",
  description = "Agent that calls connected nodes as tools",
  inputs = { "text" },
  outputs = { "text" },
  config = {
    { key = "goal", label = "Goal", type = "textarea", default = "" },
    { key = "system", label = "System", type = "textarea", default = "" },
    { key = "maxSteps", label = "Max steps", type = "number", default = 6 },
    { key = "provider", label = "Provider", type = "text", default = "" },
    { key = "model", label = "Model", type = "text", default = "" },
  },

  run = function(ctx)
    return ctx.brain{
      goal = ctx.config.goal,
      system = ctx.config.system,
      maxSteps = ctx.config.maxSteps,
      provider = ctx.config.provider,
      model = ctx.config.model,
    }
  end,
}
