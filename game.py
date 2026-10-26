import random

secret = random.sample("0123456789", 4)

while True:
    guess = input("Guess: ")
    dead = 0
    injured = 0
    for i in range(4):
        if guess[i] == secret[i]:
            dead += 1
        elif guess[i] in secret:
            injured += 1
    print(dead, "dead,", injured, "injured")
    if dead == 4:
        print("You win!")
        break
