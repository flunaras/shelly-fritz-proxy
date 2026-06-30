This proxy is intended to act as a control device for a solakon solar battery in order to archive a zero feed.
The solakon device will query the shelly proxy to regulate it's power output only to cover all local power consumption without distributing a surplus into the public network (because you wo't get paid for this).
However, the query rate from the solakon device will probably be much higher than the fritz backend device will update it's data, so this will probably lead to an unwanted escalation of the solakon power outoput like this example:
1. Solakon currently outputs 20W
2. Solakon queries the current power consumption which is currently 50W
3. Solakon increases it's power generation by 50W to balance it
4. Solakon reads again the current power consumption and expects it to be near zero, but since the fritz device hasn't updates it's data yet, it still gets a read of 50W
5. Solakon doesn't know that the data is outdated and increases it's power generation again by 50W, generating now 100W and 50W too much
6. Solakon reads again and maybe the fritz device still hasn't updated it's data and solakon increases it's output by another 50W
and so on

To counter this, I came with the idea of a "forecast mode":
Add the ability to not only query a fritz device but also read the current power supply of a solakon device (or better a configurable list of those)
Every time a query is made, query the fritz box and all configured solakon devices (the sum of all device readings). If the value of the fritz device has changed, it's probably the right current value and just give it back. Also remember remember the value as well as the current solakon value sum.
If the value didn't change from the last remembered value, compare the actual solakon value sum with the remembered one and return an adjusted fritz device value.
From the example above:
1. Solakon currently outputs 20W
2. Solakon queries the current power consumption. The fritz device has an updated value of 50W, so this is reported to solakon
3. Solakon increases it's power generation by 50W to balance it
4. Solakon reads again. The Fritz device still reads 50W, but the solakon output has increased to 70W. So the value reported to solakon will calculates with remembered_fritz_value - current_solakon_power + remembered_soaklon_power: 50W-70W+20W=0W
5. Solakon will get the value of 0W and will keep it's power at 70W

Implement such a feature:
1. There should be a configuration switch to enable/disable it
2. The type of solar battery should be pluggable with solakon as the first implementation

