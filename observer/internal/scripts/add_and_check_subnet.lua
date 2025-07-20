-- KEYS[1]: ключ множества ПОДСЕТЕЙ пользователя (например, user_subnets:tg15_32732832)
-- KEYS[2]: ключ кулдауна алертов для пользователя (например, alert_sent:tg15_32732832)
-- ARGV[1]: ПОДСЕТЬ для добавления (например, "22.3.49.0/24")
-- ARGV[2]: TTL для ПОДСЕТИ в секундах
-- ARGV[3]: Лимит ПОДСЕТЕЙ для пользователя
-- ARGV[4]: TTL для кулдауна алертов в секундах

local isNewSubnet = redis.call('SADD', KEYS[1], ARGV[1])

-- Извлекаем email из ключа KEYS[1] (user_subnets: -> 13 символов)
local userEmail = string.sub(KEYS[1], 14) 
-- Формируем ключ для TTL
local subnetTtlKey = 'subnet_ttl:' .. userEmail .. ':' .. ARGV[1]
redis.call('SETEX', subnetTtlKey, ARGV[2], '1')

redis.call('EXPIRE', KEYS[1], ARGV[2])

local currentSubnetCount = redis.call('SCARD', KEYS[1])
local subnetLimit = tonumber(ARGV[3])

if currentSubnetCount > subnetLimit then
    local alertSent = redis.call('EXISTS', KEYS[2])
    if alertSent == 0 then
        redis.call('SETEX', KEYS[2], ARGV[4], '1')
        local allSubnets = redis.call('SMEMBERS', KEYS[1])
        return {1, allSubnets}
    else
        return {2, currentSubnetCount}
    end
end

return {0, currentSubnetCount, isNewSubnet}