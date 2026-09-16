-- A node that spends one model call. The budget is enforced by the host, so a
-- mistake here costs the user one call, not their afternoon.
return {
  type = "summarize",
  label = "Summarize",
  category = "AI",
  description = "Reduce the incoming text to a few sentences",
  inputs = { "text" },
  outputs = { "text" },
  config = {
    { key = "sentences", label = "Sentences", type = "number", default = 3 },
    { key = "tone", label = "Tone", type = "select", options = { "plain", "technical", "friendly" }, default = "plain" },
  },

  run = function(ctx)
    local text = ctx.input.text
    if text == nil or text == "" then
      error("Summarize needs some text on its input")
    end

    local sentences = ctx.config.sentences or 3
    local tone = ctx.config.tone or "plain"

    return {
      text = ctx.llm{
        system = "You summarize. Reply with the summary and nothing else.",
        prompt = "Summarize the following in "
          .. sentences
          .. " sentences, in a "
          .. tone
          .. " register.\n\n"
          .. text,
        maxTokens = 400,
      },
    }
  end,
}
